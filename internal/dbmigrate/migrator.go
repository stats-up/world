package dbmigrate

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"world/internal/config"
	"world/internal/docker"
	"world/internal/store"
)

// La copia corre en un contenedor efímero con el cliente oficial de MySQL 8.4:
//
//	mysqldump (origen) | sed (compatibilidad) | mysql (MainDB)
//
// Las contraseñas van por variables de ambiente y se escriben en archivos de opciones
// dentro del contenedor (nunca en la línea de comandos). El contenedor se borra al terminar.

const (
	ClientImage = "mysql:8.4"
	migLabel    = "world.managed=dbmigrate"
	migMemory   = 512 << 20
	pollEvery   = 3 * time.Second
)

var ErrRunning = errors.New("ya hay una migración en curso hacia esa base")

// dumpOptions: copia consistente (InnoDB) sin bloquear el origen, con vistas, triggers,
// procedimientos y eventos. --column-statistics=0 y --set-gtid-purged=OFF permiten leer
// MariaDB y no exigen permisos especiales en MainDB.
const dumpOptions = "--single-transaction --quick --routines --triggers --events --hex-blob --no-tablespaces " +
	"--column-statistics=0 --set-gtid-purged=OFF --default-character-set=utf8mb4"

// sedProgram adapta el dump a MySQL 8 administrado (sin SUPER):
//   - registra cada tabla en el log (avance),
//   - quita los DEFINER (los objetos quedan a nombre del usuario de MainDB),
//   - cambia intercalaciones de MariaDB 11 que MySQL no tiene,
//   - pasa Aria/MyISAM a InnoDB y quita opciones exclusivas de Aria.
const sedProgram = "/^-- Table structure for table/w /dev/stderr\n" +
	"s/DEFINER=`[^`]*`@`[^`]*`//g\n" +
	"s/(utf8mb[34])_uca1400_[a-z0-9_]+/\\1_unicode_ci/g\n" +
	"s/ENGINE=(Aria|MyISAM)/ENGINE=InnoDB/g\n" +
	"s/ (PAGE_CHECKSUM|TRANSACTIONAL)=[01]//g\n"

// script corre dentro del contenedor. Todo lo que escribe va al log de la migración.
const script = `set -eo pipefail
exec >>"/logs/db-$MIG_ID.log" 2>&1
esc() { printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'; }
umask 077
printf '[client]\nhost=%s\nport=%s\nuser="%s"\npassword="%s"\n' "$SRC_HOST" "$SRC_PORT" "$(esc "$SRC_USER")" "$(esc "$SRC_PASS")" > /tmp/src.cnf
printf '[client]\nhost=%s\nport=%s\nuser="%s"\npassword="%s"\n' "$DST_HOST" "$DST_PORT" "$(esc "$DST_USER")" "$(esc "$DST_PASS")" > /tmp/dst.cnf
unset SRC_PASS DST_PASS
echo "== $(date '+%F %T') Copiando $SRC_DB ($SRC_HOST) → $DST_DB (MainDB)"
mysqldump --defaults-extra-file=/tmp/src.cnf $DUMP_OPTIONS "$SRC_DB" \
  | sed -E -e "$SED_PROGRAM" \
  | mysql --defaults-extra-file=/tmp/dst.cnf --default-character-set=utf8mb4 "$DST_DB"
echo "== $(date '+%F %T') Copia terminada"
`

// Request es lo que pide el formulario. La contraseña de origen no se guarda.
type Request struct {
	Source  Conn
	DstDB   string
	Charset string
	Collate string
	Tables  int
	Bytes   int64
}

type Migrator struct {
	dc     *docker.Client
	cfg    config.Config
	st     *store.Store
	target func() Conn // MainDB, leída de los ajustes en cada uso

	mu       sync.Mutex
	ctx      context.Context
	watching map[int64]bool
}

func NewMigrator(dc *docker.Client, cfg config.Config, st *store.Store, target func() Conn) *Migrator {
	return &Migrator{dc: dc, cfg: cfg, st: st, target: target, ctx: context.Background(), watching: map[int64]bool{}}
}

func containerName(id int64) string { return fmt.Sprintf("world-dbmigrate-%d", id) }

func (m *Migrator) logDir() string { return m.cfg.Path("db-migrations") }

func (m *Migrator) LogPath(id int64) string {
	return filepath.Join(m.logDir(), fmt.Sprintf("db-%d.log", id))
}

// Start crea la base en MainDB y lanza la copia en segundo plano.
func (m *Migrator) Start(ctx context.Context, req Request) (int64, error) {
	dst := m.target()
	if !Configured(dst) {
		return 0, errors.New("falta configurar la conexión a MainDB")
	}
	if prev, err := m.st.RunningDBMigrationTo(req.DstDB); err == nil && prev != nil {
		return 0, ErrRunning
	}
	if imgs, err := m.dc.ImagesByReference(ctx, ClientImage); err != nil || len(imgs) == 0 {
		if err := m.dc.ImagePull(ctx, ClientImage); err != nil {
			return 0, fmt.Errorf("descargando %s: %w", ClientImage, err)
		}
	}
	if err := os.MkdirAll(m.logDir(), 0o700); err != nil {
		return 0, err
	}
	if err := CreateDatabase(ctx, dst, req.DstDB, req.Charset, req.Collate); err != nil {
		return 0, fmt.Errorf("creando la base %s en MainDB: %w", req.DstDB, err)
	}
	id, err := m.st.CreateDBMigration(&store.DBMigration{SrcHost: req.Source.Host, SrcPort: req.Source.Port, SrcUser: req.Source.User,
		SrcDB: req.Source.Database, DstDB: req.DstDB, TablesTotal: req.Tables, BytesEst: req.Bytes})
	if err != nil {
		return 0, err
	}
	fail := func(err error) (int64, error) {
		m.st.FinishDBMigration(id, store.DBMigFailed, err.Error())
		return 0, err
	}
	env := []string{
		"MIG_ID=" + strconv.FormatInt(id, 10),
		"SRC_HOST=" + req.Source.Host, "SRC_PORT=" + strconv.Itoa(req.Source.Port), "SRC_USER=" + req.Source.User,
		"SRC_PASS=" + req.Source.Password, "SRC_DB=" + req.Source.Database,
		"DST_HOST=" + dst.Host, "DST_PORT=" + strconv.Itoa(dst.Port), "DST_USER=" + dst.User, "DST_PASS=" + dst.Password,
		"DST_DB=" + req.DstDB, "DUMP_OPTIONS=" + dumpOptions, "SED_PROGRAM=" + sedProgram,
	}
	cid, err := m.dc.ContainerCreate(ctx, containerName(id), docker.ContainerConfig{
		Image:      ClientImage,
		Entrypoint: []string{"bash", "-c"},
		Cmd:        []string{script},
		Env:        env,
		Labels:     map[string]string{"world.managed": "dbmigrate", "world.dbmigration-id": strconv.FormatInt(id, 10)},
		HostConfig: docker.HostConfig{
			Binds:    []string{m.logDir() + ":/logs"},
			Memory:   migMemory,
			NanoCPUs: 1e9,
		},
	})
	if err != nil {
		return fail(fmt.Errorf("creando el contenedor: %w", err))
	}
	if err := m.dc.ContainerStart(ctx, cid); err != nil {
		m.dc.ContainerRemove(ctx, cid)
		return fail(fmt.Errorf("iniciando la copia: %w", err))
	}
	m.watch(id)
	return id, nil
}

// Cancel detiene la copia. La base de destino queda a medias (se puede vaciar desde el panel).
func (m *Migrator) Cancel(ctx context.Context, id int64) error {
	if err := m.st.FinishDBMigration(id, store.DBMigCanceled, ""); err != nil {
		return err
	}
	name := containerName(id)
	if err := m.dc.ContainerStop(ctx, name, 5*time.Second); err != nil && !docker.IsNotFound(err) {
		log.Printf("dbmigrate %d: deteniendo: %v", id, err)
	}
	if err := m.dc.ContainerRemove(ctx, name); err != nil && !docker.IsNotFound(err) {
		return err
	}
	return nil
}

// Resume retoma el seguimiento tras reiniciar el panel y quita contenedores huérfanos.
func (m *Migrator) Resume(ctx context.Context) {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	running, err := m.st.RunningDBMigrations()
	if err != nil {
		log.Printf("dbmigrate: %v", err)
		return
	}
	alive := map[string]bool{}
	for _, mig := range running {
		alive[containerName(mig.ID)] = true
		m.watch(mig.ID)
	}
	containers, err := m.dc.ContainersByLabel(ctx, migLabel)
	if err != nil {
		return
	}
	for _, c := range containers {
		if len(c.Names) > 0 && !alive[c.Names[0][1:]] {
			log.Printf("dbmigrate: quitando contenedor huérfano %s", c.Names[0])
			m.dc.ContainerRemove(ctx, c.ID)
		}
	}
}

func (m *Migrator) watch(id int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.watching[id] {
		return
	}
	m.watching[id] = true
	go m.follow(m.ctx, id)
}

func (m *Migrator) follow(ctx context.Context, id int64) {
	defer func() {
		m.mu.Lock()
		delete(m.watching, id)
		m.mu.Unlock()
	}()
	name := containerName(id)
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		info, err := m.dc.ContainerInspect(ctx, name)
		if docker.IsNotFound(err) {
			m.st.FinishDBMigration(id, store.DBMigFailed, "Se interrumpió (se reinició el servidor o se quitó el contenedor). Vacía la base de destino y migra de nuevo.")
			return
		}
		if err == nil {
			m.st.SetDBMigrationProgress(id, max(0, CountTables(m.Log(id))-1)) // la última sigue copiándose
		}
		if err == nil && !info.State.Running && info.State.Status != "created" {
			m.finish(id, info)
			rmCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			m.dc.ContainerRemove(rmCtx, info.ID)
			cancel()
			return
		}
		select {
		case <-ctx.Done():
			return // el panel se apaga: el contenedor sigue y Resume lo retoma
		case <-t.C:
		}
	}
}

// finish revisa el resultado: código de salida y que MainDB tenga todas las tablas.
func (m *Migrator) finish(id int64, info *docker.ContainerInfo) {
	mig, err := m.st.DBMigration(id)
	if err != nil || !mig.Running() {
		return
	}
	if code := info.State.ExitCode; code != 0 {
		msg := ExitMessage(code, m.Log(id))
		if info.State.OOMKilled {
			msg = "La copia se quedó sin memoria. Vacía la base de destino y migra de nuevo."
		}
		m.st.FinishDBMigration(id, store.DBMigFailed, msg)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	n, err := TargetTables(ctx, m.target(), mig.DstDB)
	switch {
	case err != nil:
		m.st.SetDBMigrationProgress(id, mig.TablesTotal)
		m.st.FinishDBMigration(id, store.DBMigSuccess, "La copia terminó bien, pero no se pudo verificar en MainDB: "+err.Error())
	case n < mig.TablesTotal:
		m.st.SetDBMigrationProgress(id, n)
		m.st.FinishDBMigration(id, store.DBMigFailed, fmt.Sprintf("En MainDB hay %d de %d tablas. Revisa el log.", n, mig.TablesTotal))
	default:
		m.st.SetDBMigrationProgress(id, n)
		m.st.FinishDBMigration(id, store.DBMigSuccess, "")
	}
}

// Log devuelve el final del log de la migración.
func (m *Migrator) Log(id int64) string {
	f, err := os.Open(m.LogPath(id))
	if err != nil {
		return ""
	}
	defer f.Close()
	const max = 256 << 10
	if st, err := f.Stat(); err == nil && st.Size() > max {
		f.Seek(-max, io.SeekEnd)
	}
	b, _ := io.ReadAll(f)
	return string(b)
}

var tableLineRe = regexp.MustCompile("(?m)^-- Table structure for table `")

// CountTables cuenta las tablas que el dump ya empezó a copiar.
func CountTables(log string) int { return len(tableLineRe.FindAllStringIndex(log, -1)) }

var mysqlErrRe = regexp.MustCompile(`(?m)^(?:mysqldump: )?(?:Got error: |ERROR )(\d+)[^:]*: (.*)$`)

// ExitMessage explica por qué falló la copia, a partir del último error del log.
func ExitMessage(code int, logText string) string {
	var last []string
	sc := bufio.NewScanner(strings.NewReader(logText))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if mm := mysqlErrRe.FindStringSubmatch(sc.Text()); mm != nil {
			last = mm
		}
	}
	if last == nil {
		if code == 137 || code == 143 {
			return "La copia se detuvo antes de terminar."
		}
		return fmt.Sprintf("La copia terminó con código %d. Revisa el log.", code)
	}
	detail := strings.TrimSpace(last[2])
	switch last[1] {
	case "1419", "1418":
		return "MainDB no permite crear triggers o funciones con el registro binario activo. En Lightsail: base de datos → Parámetros → log_bin_trust_function_creators = 1, y migra de nuevo. (" + detail + ")"
	case "1227":
		return "MainDB rechazó una instrucción que exige privilegios de administrador (SUPER). " + detail
	case "1045":
		return "Usuario o contraseña incorrectos. " + detail
	case "2002", "2003", "2005", "2013":
		return "Se perdió o no se pudo abrir la conexión. " + detail
	case "1273":
		return "MainDB no conoce una intercalación (collation) del origen. " + detail
	}
	return "Error " + last[1] + ": " + detail
}
