// Package backup respalda la base de datos MySQL/MariaDB de cada sitio que lo tenga activado:
// a la hora programada (hora de Chile) o a pedido, en el disco del servidor, en un bucket S3 o en ambos.
//
// El respaldo corre en un contenedor efímero con el cliente oficial de MySQL (mysqldump | gzip).
// Los datos de conexión salen de las variables del sitio (DB_HOST, DB_DATABASE, …) y la contraseña
// va en un archivo de opciones dentro del contenedor, nunca en la línea de comandos.
package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // America/Santiago aunque el servidor no tenga zonas horarias instaladas

	"world/internal/bucket"
	"world/internal/config"
	"world/internal/dbmigrate"
	"world/internal/deploy"
	"world/internal/docker"
	"world/internal/proxy"
	"world/internal/secret"
	"world/internal/store"
)

const (
	label      = "world.managed=backup"
	memory     = 512 << 20
	runTimeout = 3 * time.Hour
	// Prefijo de los archivos en el bucket: world-backups/<sitio>/<archivo>.
	bucketPrefix = "world-backups"
	// Se conservan los últimos fallidos para ver qué pasó; los más antiguos se borran.
	keepFailed = 20
)

// dumpOptions: copia consistente (InnoDB) sin bloquear la base y sin pedir permisos que un usuario
// de aplicación no suele tener (rutinas, eventos, PROCESS). Cada tabla va con DROP TABLE IF EXISTS,
// así que restaurar reemplaza las tablas del respaldo.
const dumpOptions = "--single-transaction --quick --triggers --hex-blob --no-tablespaces " +
	"--column-statistics=0 --set-gtid-purged=OFF --default-character-set=utf8mb4"

const cnfScript = `set -eo pipefail
umask 077
esc() { printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'; }
printf '[client]\nhost=%s\nport=%s\nuser="%s"\npassword="%s"\n' "$DB_HOST" "$DB_PORT" "$(esc "$DB_USER")" "$(esc "$DB_PASS")" > /tmp/db.cnf
unset DB_PASS
`

const dumpScript = cnfScript + `mysqldump --defaults-extra-file=/tmp/db.cnf $DUMP_OPTIONS "$DB_NAME" | gzip -6 > "/out/$FILE.part"
mv "/out/$FILE.part" "/out/$FILE"
`

const restoreScript = cnfScript + `gunzip -c "/out/$FILE" | mysql --defaults-extra-file=/tmp/db.cnf --default-character-set=utf8mb4 "$DB_NAME"
`

// Target: la base de datos del sitio, leída de sus variables de ambiente.
type Target struct {
	Host, User, Password, Database string
	Port                           int
}

// TargetOf lee la conexión de las variables del sitio (las mismas que usa Laravel).
func TargetOf(env []string) (Target, error) {
	vars := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		vars[k] = v
	}
	conn := vars["DB_CONNECTION"]
	if conn != "" && conn != "mysql" && conn != "mariadb" {
		return Target{}, fmt.Errorf("el sitio usa DB_CONNECTION=%s: solo se respaldan bases MySQL/MariaDB", conn)
	}
	t := Target{Host: vars["DB_HOST"], User: vars["DB_USERNAME"], Password: vars["DB_PASSWORD"], Database: vars["DB_DATABASE"], Port: 3306}
	if p := vars["DB_PORT"]; p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 || n > 65535 {
			return Target{}, fmt.Errorf("DB_PORT inválido: %s", p)
		}
		t.Port = n
	}
	var missing []string
	for k, v := range map[string]string{"DB_HOST": t.Host, "DB_DATABASE": t.Database, "DB_USERNAME": t.User} {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return Target{}, fmt.Errorf("faltan variables de la base de datos en el sitio: %s", strings.Join(sortStrings(missing), ", "))
	}
	if !dbmigrate.ValidDBName(t.Database) {
		return Target{}, fmt.Errorf("nombre de base inválido: %s", t.Database)
	}
	if t.Host == "127.0.0.1" || t.Host == "localhost" {
		return Target{}, errors.New("DB_HOST apunta al propio contenedor (localhost): world no puede llegar a esa base")
	}
	return t, nil
}

func sortStrings(s []string) []string {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
	return s
}

// Manager programa y ejecuta los respaldos. Corre uno a la vez para no cargar el servidor ni la base.
type Manager struct {
	dc   *docker.Client
	cfg  config.Config
	st   *store.Store
	box  *secret.Box
	http *http.Client
	Loc  *time.Location

	run  chan struct{} // uno a la vez
	mu   sync.Mutex
	busy map[int64]string // sitio → "backup" | "restore"
}

func New(dc *docker.Client, cfg config.Config, st *store.Store, box *secret.Box) *Manager {
	loc, err := time.LoadLocation("America/Santiago")
	if err != nil {
		loc = time.Local
	}
	return &Manager{dc: dc, cfg: cfg, st: st, box: box, http: &http.Client{Timeout: runTimeout}, Loc: loc,
		run: make(chan struct{}, 1), busy: map[int64]string{}}
}

// Dir: carpeta de los respaldos de un sitio en el disco (por id, así sobrevive a un cambio de nombre).
func (m *Manager) Dir(siteID int64) string {
	return m.cfg.Path("backups", strconv.FormatInt(siteID, 10))
}

// Busy: qué está haciendo el sitio ("backup", "restore" o "").
func (m *Manager) Busy(siteID int64) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.busy[siteID]
}

func (m *Manager) claim(siteID int64, what string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.busy[siteID] != "" {
		return false
	}
	m.busy[siteID] = what
	return true
}

func (m *Manager) release(siteID int64) {
	m.mu.Lock()
	delete(m.busy, siteID)
	m.mu.Unlock()
}

// ErrBusy: ya hay un respaldo o una restauración en curso para el sitio.
var ErrBusy = errors.New("ya hay un respaldo o una restauración en curso para este sitio")

// Target lee la base del sitio desde sus variables cifradas.
func (m *Manager) Target(site *store.Site) (Target, error) {
	plain, err := m.box.OpenString(site.EnvEnc)
	if err != nil {
		return Target{}, errors.New("no se pudieron descifrar las variables del sitio")
	}
	env, _ := deploy.ParseEnv(plain)
	return TargetOf(env)
}

// Creds descifra las llaves del bucket.
func (m *Manager) Creds(bucketID int64) (bucket.Creds, error) {
	b, err := m.st.Bucket(bucketID)
	if err != nil {
		return bucket.Creds{}, errors.New("el bucket elegido ya no está en world")
	}
	sec, err := m.box.OpenString(b.SecretEnc)
	if err != nil {
		return bucket.Creds{}, errors.New("no se pudo descifrar la llave secreta del bucket")
	}
	return bucket.Creds{Bucket: b.Bucket, Region: b.Region, Endpoint: b.Endpoint, AccessKey: b.AccessKey, SecretKey: sec}, nil
}

// CheckBucket confirma que las llaves pueden escribir y borrar (sube y borra un archivo de prueba).
func (m *Manager) CheckBucket(ctx context.Context, bucketID int64) error {
	c, err := m.Creds(bucketID)
	if err != nil {
		return err
	}
	key := bucketPrefix + "/.prueba-world"
	if err := bucket.Put(ctx, m.http, c, key, strings.NewReader("ok"), 2); err != nil {
		return fmt.Errorf("no se pudo escribir en el bucket: %w", err)
	}
	if err := bucket.Delete(ctx, m.http, c, key); err != nil {
		return fmt.Errorf("se pudo escribir pero no borrar en el bucket (las copias antiguas no se podrían eliminar): %w", err)
	}
	return nil
}

// Start lanza un respaldo en segundo plano y devuelve su id.
func (m *Manager) Start(site *store.Site, source string) (int64, error) {
	if !m.claim(site.ID, "backup") {
		return 0, ErrBusy
	}
	t, err := m.Target(site)
	if err != nil {
		m.release(site.ID)
		return 0, err
	}
	id, err := m.st.CreateBackup(&store.Backup{SiteID: site.ID, SiteName: site.Name, DBName: t.Database, Source: source})
	if err != nil {
		m.release(site.ID)
		return 0, err
	}
	go func() {
		defer m.release(site.ID)
		m.backup(context.Background(), site, t, id, true)
	}()
	return id, nil
}

// backup hace el respaldo id y lo deja en su destino; con prune, luego aplica la retención.
func (m *Manager) backup(ctx context.Context, site *store.Site, t Target, id int64, prune bool) *store.Backup {
	m.run <- struct{}{}
	defer func() { <-m.run }()
	cfg, err := m.st.BackupConfig(site.ID)
	if err != nil {
		cfg = store.DefaultBackupConfig(site.ID)
	}
	b := &store.Backup{ID: id, SiteID: site.ID, Status: store.BackupFailed}
	defer func() {
		if err := m.st.FinishBackup(b); err != nil {
			log.Printf("backup %d: %v", id, err)
		}
	}()
	dir := m.Dir(site.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		b.Error = err.Error()
		return b
	}
	b.File = fmt.Sprintf("%s-%s.sql.gz", site.Name, time.Now().In(m.Loc).Format("20060102-150405"))
	path := filepath.Join(dir, b.File)
	if err := m.container(ctx, "backup", id, dir, dumpScript, t, b.File); err != nil {
		os.Remove(path + ".part")
		b.File, b.Error = "", err.Error()
		return b
	}
	if st, err := os.Stat(path); err == nil {
		b.Bytes = st.Size()
	}
	b.Status, b.OnDisk = store.BackupOK, true

	if cfg.ToBucket() && cfg.BucketID > 0 {
		key := bucketPrefix + "/" + site.Name + "/" + b.File
		if err := m.upload(ctx, cfg.BucketID, key, path, b.Bytes); err != nil {
			b.Error = "No se pudo subir al bucket (la copia quedó en el disco del servidor): " + err.Error()
		} else {
			b.BucketID, b.BucketKey = cfg.BucketID, key
			if !cfg.ToDisk() {
				os.Remove(path)
				b.OnDisk = false
			}
		}
	}
	if prune {
		m.st.FinishBackup(b)
		m.prune(ctx, site.ID, cfg.Keep)
	}
	return b
}

func (m *Manager) upload(ctx context.Context, bucketID int64, key, path string, size int64) error {
	c, err := m.Creds(bucketID)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return bucket.Put(ctx, m.http, c, key, f, size)
}

// prune deja las últimas keep copias correctas y los últimos fallidos.
func (m *Manager) prune(ctx context.Context, siteID int64, keep int) {
	list, err := m.st.Backups(siteID, 10000)
	if err != nil {
		return
	}
	ok, failed := 0, 0
	for _, b := range list {
		switch {
		case b.Running():
		case b.OK():
			if ok++; ok > keep {
				if err := m.Remove(ctx, b); err != nil {
					log.Printf("backup %d: borrando copia antigua: %v", b.ID, err)
				}
			}
		default:
			if failed++; failed > keepFailed {
				m.st.DeleteBackup(b.ID)
			}
		}
	}
}

// Remove borra los archivos del respaldo (disco y bucket) y su registro.
func (m *Manager) Remove(ctx context.Context, b *store.Backup) error {
	if b.OnDisk && b.File != "" {
		if err := os.Remove(filepath.Join(m.Dir(b.SiteID), b.File)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if b.BucketKey != "" {
		c, err := m.Creds(b.BucketID)
		if err != nil {
			return fmt.Errorf("%w; el archivo sigue en el bucket: %s", err, b.BucketKey)
		}
		if err := bucket.Delete(ctx, m.http, c, b.BucketKey); err != nil {
			return err
		}
	}
	return m.st.DeleteBackup(b.ID)
}

// Open abre el archivo del respaldo para descargarlo (primero el disco; si no está, el bucket).
func (m *Manager) Open(ctx context.Context, b *store.Backup) (io.ReadCloser, int64, error) {
	if b.OnDisk {
		f, err := os.Open(filepath.Join(m.Dir(b.SiteID), b.File))
		if err == nil {
			st, _ := f.Stat()
			return f, st.Size(), nil
		}
		if b.BucketKey == "" {
			return nil, 0, errors.New("el archivo ya no está en el disco")
		}
	}
	if b.BucketKey == "" {
		return nil, 0, errors.New("el respaldo no tiene archivo")
	}
	c, err := m.Creds(b.BucketID)
	if err != nil {
		return nil, 0, err
	}
	return bucket.Get(ctx, m.http, c, b.BucketKey)
}

// Restore reemplaza las tablas de la base del sitio con las del respaldo, en segundo plano.
// Antes respalda el estado actual (por si hay que volver atrás).
func (m *Manager) Restore(site *store.Site, b *store.Backup) error {
	if !b.OK() {
		return errors.New("solo se pueden restaurar respaldos correctos")
	}
	if !m.claim(site.ID, "restore") {
		return ErrBusy
	}
	t, err := m.Target(site)
	if err != nil {
		m.release(site.ID)
		return err
	}
	if t.Database != b.DBName {
		m.release(site.ID)
		return fmt.Errorf("el respaldo es de la base %s y el sitio ahora usa %s: restáuralo a mano desde la pestaña SQL", b.DBName, t.Database)
	}
	go func() {
		defer m.release(site.ID)
		ctx := context.Background()
		msg := ""
		if err := m.restore(ctx, site, t, b); err != nil {
			msg = err.Error()
			log.Printf("restaurando backup %d: %v", b.ID, err)
		}
		m.st.SetBackupRestored(b.ID, msg)
	}()
	return nil
}

func (m *Manager) restore(ctx context.Context, site *store.Site, t Target, b *store.Backup) error {
	pre, err := m.st.CreateBackup(&store.Backup{SiteID: site.ID, SiteName: site.Name, DBName: t.Database, Source: "pre-restore"})
	if err != nil {
		return err
	}
	// Sin retención: así no se borra justo el respaldo que se va a restaurar.
	if r := m.backup(ctx, site, t, pre, false); !r.OK() {
		return fmt.Errorf("no se restauró: falló el respaldo previo del estado actual (%s)", r.Error)
	}
	m.run <- struct{}{}
	defer func() { <-m.run }()
	dir := m.Dir(site.ID)
	file := b.File
	if !b.OnDisk || !exists(filepath.Join(dir, file)) {
		// Viene del bucket: se descarga a un archivo temporal que se borra al terminar.
		file = fmt.Sprintf(".restore-%d.sql.gz", b.ID)
		tmp := filepath.Join(dir, file)
		defer os.Remove(tmp)
		if err := m.download(ctx, b, tmp); err != nil {
			return fmt.Errorf("descargando el respaldo del bucket: %w", err)
		}
	}
	return m.container(ctx, "restore", b.ID, dir, restoreScript, t, file)
}

func (m *Manager) download(ctx context.Context, b *store.Backup, path string) error {
	c, err := m.Creds(b.BucketID)
	if err != nil {
		return err
	}
	body, _, err := bucket.Get(ctx, m.http, c, b.BucketKey)
	if err != nil {
		return err
	}
	defer body.Close()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// container corre script en un contenedor efímero con dir montado en /out y espera a que termine.
func (m *Manager) container(ctx context.Context, kind string, id int64, dir, script string, t Target, file string) error {
	if imgs, err := m.dc.ImagesByReference(ctx, dbmigrate.ClientImage); err != nil || len(imgs) == 0 {
		if err := m.dc.ImagePull(ctx, dbmigrate.ClientImage); err != nil {
			return fmt.Errorf("descargando %s: %w", dbmigrate.ClientImage, err)
		}
	}
	name := fmt.Sprintf("world-%s-%d", kind, id)
	m.dc.ContainerRemove(ctx, name) // restos de un intento anterior
	cid, err := m.dc.ContainerCreate(ctx, name, docker.ContainerConfig{
		Image:      dbmigrate.ClientImage,
		Entrypoint: []string{"bash", "-c"},
		Cmd:        []string{script},
		Env: []string{"DB_HOST=" + t.Host, "DB_PORT=" + strconv.Itoa(t.Port), "DB_USER=" + t.User, "DB_PASS=" + t.Password,
			"DB_NAME=" + t.Database, "FILE=" + file, "DUMP_OPTIONS=" + dumpOptions},
		Labels: map[string]string{"world.managed": "backup"},
		HostConfig: docker.HostConfig{
			Binds:    []string{dir + ":/out"},
			Memory:   memory,
			NanoCPUs: 1e9,
		},
		// En la red de los sitios, para llegar también a bases que corren en otro contenedor.
		NetworkingConfig: &docker.NetworkingConfig{EndpointsConfig: map[string]docker.EndpointSettings{proxy.Network: {}}},
	})
	if err != nil {
		return fmt.Errorf("creando el contenedor: %w", err)
	}
	defer func() {
		rmCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		m.dc.ContainerRemove(rmCtx, cid)
		cancel()
	}()
	if err := m.dc.ContainerStart(ctx, cid); err != nil {
		return fmt.Errorf("iniciando el contenedor: %w", err)
	}
	deadline := time.Now().Add(runTimeout)
	for {
		info, err := m.dc.ContainerInspect(ctx, cid)
		if err != nil {
			return err
		}
		if !info.State.Running && info.State.Status != "created" {
			if info.State.ExitCode == 0 {
				return nil
			}
			logs, _ := m.dc.ContainerLogs(ctx, cid, 20)
			return fmt.Errorf("%s (código %d)", lastLines(logs, 6), info.State.ExitCode)
		}
		if time.Now().After(deadline) {
			m.dc.ContainerStop(ctx, cid, 5*time.Second)
			return fmt.Errorf("no terminó en %s y se detuvo", runTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// lastLines deja las últimas n líneas no vacías del log, sin la marca de tiempo de Docker.
func lastLines(logs string, n int) string {
	var out []string
	for _, l := range strings.Split(logs, "\n") {
		if _, rest, ok := strings.Cut(l, "Z "); ok && len(l) > 20 && l[4] == '-' {
			l = rest
		}
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	if len(out) == 0 {
		return "el contenedor terminó con error y sin mensajes"
	}
	return strings.Join(out, " · ")
}

// Run revisa cada minuto qué sitios tienen un respaldo programado para esta hora.
func (m *Manager) Run(ctx context.Context) {
	if err := m.st.FailInterruptedBackups(); err != nil {
		log.Printf("backups: %v", err)
	}
	m.cleanup(ctx)
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		m.tick(time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (m *Manager) tick(now time.Time) {
	cfgs, err := m.st.EnabledBackupConfigs()
	if err != nil {
		log.Printf("backups: %v", err)
		return
	}
	local := now.In(m.Loc)
	hour := local.Truncate(time.Hour)
	for _, c := range cfgs {
		if !c.Due(local) || !m.st.LastAutoBackup(c.SiteID).In(m.Loc).Before(hour) {
			continue
		}
		site, err := m.st.Site(c.SiteID)
		if err != nil {
			continue // el sitio se eliminó
		}
		if _, err := m.Start(site, "auto"); err != nil && !errors.Is(err, ErrBusy) {
			// Queda registrado como fallido para que se vea en el panel.
			if id, cerr := m.st.CreateBackup(&store.Backup{SiteID: site.ID, SiteName: site.Name, Source: "auto"}); cerr == nil {
				m.st.FinishBackup(&store.Backup{ID: id, Status: store.BackupFailed, Error: err.Error()})
			}
		}
	}
}

// cleanup quita contenedores y archivos a medias que quedaron si el panel se detuvo durante un respaldo.
func (m *Manager) cleanup(ctx context.Context) {
	if list, err := m.dc.ContainersByLabel(ctx, label); err == nil {
		for _, c := range list {
			m.dc.ContainerRemove(ctx, c.ID)
		}
	}
	parts, _ := filepath.Glob(m.cfg.Path("backups", "*", "*.part"))
	tmps, _ := filepath.Glob(m.cfg.Path("backups", "*", ".restore-*"))
	for _, p := range append(parts, tmps...) {
		os.Remove(p)
	}
}

// DiskUsage: bytes que ocupan en el disco los respaldos del sitio.
func (m *Manager) DiskUsage(siteID int64) int64 {
	var total int64
	entries, _ := os.ReadDir(m.Dir(siteID))
	for _, e := range entries {
		if info, err := e.Info(); err == nil && !e.IsDir() {
			total += info.Size()
		}
	}
	return total
}

// ForgetSite borra los backups del sitio en el disco y sus registros (al eliminar el sitio).
// Las copias en buckets se conservan: quedan fuera del servidor por si se necesitan.
func (m *Manager) ForgetSite(siteID int64) error {
	if !m.claim(siteID, "delete") {
		return ErrBusy
	}
	defer m.release(siteID)
	if err := os.RemoveAll(m.Dir(siteID)); err != nil {
		return err
	}
	return m.st.DeleteSiteBackups(siteID)
}
