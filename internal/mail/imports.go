package mail

import (
	"context"
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
	"world/internal/proxy"
	"world/internal/store"
)

// Importación de correos desde otro servidor (ej: el cPanel anterior) con imapsync.
// Cada importación es un contenedor efímero en la red de world que lee el origen por IMAP
// y escribe en Stalwart entrando como administrador ("buzón%admin"), así no hace falta
// la contraseña del buzón de destino. Repetirla no duplica: imapsync salta lo que ya está.

const (
	ImapsyncImage = "gilleslamiral/imapsync"
	importLabel   = "world.managed=imapsync"
	importMemory  = 512 << 20
	nobodyUID     = 65534 // usuario de la imagen de imapsync
	pollEvery     = 3 * time.Second
)

// ErrImportRunning: ya hay una importación corriendo para ese buzón.
var ErrImportRunning = fmt.Errorf("ya hay una importación en curso para este buzón")

// ImportRequest es lo que pide el formulario. La contraseña de origen no se guarda:
// solo viaja al contenedor mientras corre (y el contenedor se borra al terminar).
type ImportRequest struct {
	AccountID string
	Email     string // buzón de destino en Stalwart
	Host      string
	Port      int
	User      string
	Password  string
}

type Importer struct {
	dc     *docker.Client
	cfg    config.Config
	st     *store.Store
	secret func() string // contraseña del administrador de Stalwart

	mu       sync.Mutex
	ctx      context.Context
	watching map[int64]bool
}

func NewImporter(dc *docker.Client, cfg config.Config, st *store.Store, adminSecret func() string) *Importer {
	return &Importer{dc: dc, cfg: cfg, st: st, secret: adminSecret, ctx: context.Background(), watching: map[int64]bool{}}
}

func importContainer(id int64) string { return fmt.Sprintf("world-imapsync-%d", id) }

func (im *Importer) logDir() string { return im.cfg.Path("mail-imports") }

// LogPath del log de imapsync de una importación (queda guardado al terminar).
func (im *Importer) LogPath(id int64) string {
	return filepath.Join(im.logDir(), fmt.Sprintf("import-%d.log", id))
}

// importArgs arma la línea de imapsync. Las contraseñas van por variables de ambiente
// (IMAPSYNC_PASSWORD1/2), no en la línea de comandos.
func importArgs(req ImportRequest, id int64) []string {
	// La imagen no tiene ENTRYPOINT: el comando reemplaza al CMD, así que va el binario primero.
	args := []string{"/usr/bin/imapsync", "--host1", req.Host, "--port1", strconv.Itoa(req.Port), "--user1", req.User}
	if req.Port == 993 {
		args = append(args, "--ssl1")
	} else {
		args = append(args, "--tls1")
	}
	return append(args,
		"--host2", ContainerName, "--port2", "993", "--ssl2", "--user2", req.Email+"%"+AdminUser,
		"--automap", // Enviados, Papelera, etc. van a las carpetas equivalentes de Stalwart
		"--logdir", "/logs", "--logfile", fmt.Sprintf("import-%d.log", id),
		"--noreleasecheck", "--no-modulesversion",
	)
}

// Start lanza una importación en segundo plano y devuelve su id.
func (im *Importer) Start(ctx context.Context, req ImportRequest) (int64, error) {
	secret := im.secret()
	if secret == "" {
		return 0, fmt.Errorf("falta la contraseña de administrador de Stalwart (Ajustes → Correo)")
	}
	if info, err := im.dc.ContainerInspect(ctx, ContainerName); err != nil || !info.State.Running {
		return 0, ErrNotRunning
	}
	if prev, err := im.st.MailImports(req.AccountID, 1); err == nil && len(prev) > 0 && prev[0].Running() {
		return 0, ErrImportRunning
	}
	if imgs, err := im.dc.ImagesByReference(ctx, ImapsyncImage); err != nil || len(imgs) == 0 {
		if err := im.dc.ImagePull(ctx, ImapsyncImage); err != nil {
			return 0, fmt.Errorf("descargando imapsync: %w", err)
		}
	}
	if err := os.MkdirAll(im.logDir(), 0o700); err != nil {
		return 0, err
	}
	if err := os.Chown(im.logDir(), nobodyUID, nobodyUID); err != nil && os.Geteuid() == 0 {
		return 0, err
	}

	id, err := im.st.CreateMailImport(&store.MailImport{AccountID: req.AccountID, Email: req.Email,
		Host: req.Host, Port: req.Port, SrcUser: req.User})
	if err != nil {
		return 0, err
	}
	fail := func(err error) (int64, error) {
		im.st.FinishMailImport(id, store.ImportFailed, err.Error())
		return 0, err
	}
	cid, err := im.dc.ContainerCreate(ctx, importContainer(id), docker.ContainerConfig{
		Image:  ImapsyncImage,
		Cmd:    importArgs(req, id),
		Env:    []string{"IMAPSYNC_PASSWORD1=" + req.Password, "IMAPSYNC_PASSWORD2=" + secret},
		Labels: map[string]string{"world.managed": "imapsync", "world.import-id": strconv.FormatInt(id, 10)},
		HostConfig: docker.HostConfig{
			Binds:    []string{im.logDir() + ":/logs"},
			Memory:   importMemory,
			NanoCPUs: 1e9, // 1 CPU: que no le quite aire a Stalwart
		},
		NetworkingConfig: &docker.NetworkingConfig{EndpointsConfig: map[string]struct{}{proxy.Network: {}}},
	})
	if err != nil {
		return fail(fmt.Errorf("creando el contenedor de imapsync: %w", err))
	}
	if err := im.dc.ContainerStart(ctx, cid); err != nil {
		im.dc.ContainerRemove(ctx, cid)
		return fail(fmt.Errorf("iniciando imapsync: %w", err))
	}
	im.watch(id)
	return id, nil
}

// Cancel detiene una importación en curso. Lo ya copiado queda en el buzón.
func (im *Importer) Cancel(ctx context.Context, id int64) error {
	if err := im.st.FinishMailImport(id, store.ImportCanceled, ""); err != nil {
		return err
	}
	name := importContainer(id)
	if err := im.dc.ContainerStop(ctx, name, 5*time.Second); err != nil && !docker.IsNotFound(err) {
		log.Printf("imapsync %d: deteniendo: %v", id, err)
	}
	if err := im.dc.ContainerRemove(ctx, name); err != nil && !docker.IsNotFound(err) {
		return err
	}
	return nil
}

// Resume retoma el seguimiento de las importaciones que seguían corriendo cuando el panel
// se reinició (los contenedores no dependen del panel) y borra contenedores huérfanos.
func (im *Importer) Resume(ctx context.Context) {
	im.mu.Lock()
	im.ctx = ctx
	im.mu.Unlock()

	running, err := im.st.RunningMailImports()
	if err != nil {
		log.Printf("imapsync: %v", err)
		return
	}
	alive := map[string]bool{}
	for _, m := range running {
		alive[importContainer(m.ID)] = true
		im.watch(m.ID)
	}
	containers, err := im.dc.ContainersByLabel(ctx, importLabel)
	if err != nil {
		return
	}
	for _, c := range containers {
		if len(c.Names) > 0 && !alive[c.Names[0][1:]] {
			log.Printf("imapsync: quitando contenedor huérfano %s", c.Names[0])
			im.dc.ContainerRemove(ctx, c.ID)
		}
	}
}

func (im *Importer) watch(id int64) {
	im.mu.Lock()
	defer im.mu.Unlock()
	if im.watching[id] {
		return
	}
	im.watching[id] = true
	go im.follow(im.ctx, id)
}

// follow actualiza el avance mientras el contenedor corre y cierra la importación cuando termina.
func (im *Importer) follow(ctx context.Context, id int64) {
	defer func() {
		im.mu.Lock()
		delete(im.watching, id)
		im.mu.Unlock()
	}()
	name := importContainer(id)
	tr := &logTracker{}
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		info, err := im.dc.ContainerInspect(ctx, name)
		if docker.IsNotFound(err) {
			// Cancelada (ya está cerrada y esto no la pisa) o borrada por fuera / reinicio del servidor.
			im.saveProgress(id, tr)
			im.st.FinishMailImport(id, store.ImportFailed,
				"Se interrumpió (se reinició el servidor o se quitó el contenedor). Sincroniza de nuevo: lo ya copiado no se duplica.")
			return
		}
		if err == nil && !info.State.Running && info.State.Status != "created" {
			sum := im.saveProgress(id, tr)
			status, msg := store.ImportSuccess, ""
			if code := info.State.ExitCode; code != 0 {
				status, msg = store.ImportFailed, exitMessage(code, sum.Errors)
				if info.State.OOMKilled {
					msg = "imapsync se quedó sin memoria (un correo muy grande). Sincroniza de nuevo para retomar."
				}
			}
			im.st.FinishMailImport(id, status, msg)
			rmCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			im.dc.ContainerRemove(rmCtx, info.ID)
			cancel()
			return
		}
		if err == nil {
			im.saveProgress(id, tr)
		}
		select {
		case <-ctx.Done():
			return // el panel se apaga: el contenedor sigue y Resume lo retoma
		case <-t.C:
		}
	}
}

// saveProgress lee lo nuevo del log (desde la última posición) y guarda el avance.
func (im *Importer) saveProgress(id int64, tr *logTracker) Summary {
	if f, err := os.Open(im.LogPath(id)); err == nil {
		tr.read(f)
		f.Close()
	}
	sum := tr.sum
	if sum.Total > 0 || sum.Transferred > 0 || sum.Done {
		im.st.SetMailImportProgress(id, sum.Processed, sum.Transferred, sum.Total, sum.Bytes)
	}
	return sum
}

func (im *Importer) tail(id int64, max int64) string {
	f, err := os.Open(im.LogPath(id))
	if err != nil {
		return ""
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > max {
		f.Seek(-max, io.SeekEnd)
	}
	b, _ := io.ReadAll(f)
	return string(b)
}

// Log devuelve el final del log de imapsync para mostrarlo en el panel.
func (im *Importer) Log(id int64) string { return im.tail(id, 48<<10) }

// Summary es lo que se saca del log de imapsync.
type Summary struct {
	Processed   int64 // correos del origen ya revisados (copiados o que ya estaban)
	Total       int64 // correos en el origen
	Transferred int64 // correos copiados (migrados)
	Bytes       int64 // bytes copiados
	ETA         int64 // segundos que estima imapsync para terminar
	Done        bool  // apareció el resumen final
	Errors      int64
}

var (
	etaRe         = regexp.MustCompile(`ETA: .*\s(\d+) s\s+(\d+)/(\d+) msgs left`)
	copiedRe      = regexp.MustCompile(`^msg .* copied to .*\s([\d.]+) (B|KiB|MiB|GiB|TiB) copied`)
	nbMessagesRe  = regexp.MustCompile(`Host1 Nb messages:\s+(\d+) messages`)
	transferredRe = regexp.MustCompile(`^Messages transferred\s+:\s+(\d+)`)
	bytesRe       = regexp.MustCompile(`^Total bytes transferred\s+:\s+(\d+)`)
	errorsRe      = regexp.MustCompile(`^Detected (\d+) errors`)
)

var units = map[string]float64{"B": 1, "KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40}

func atoi(s string) int64 { n, _ := strconv.ParseInt(s, 10, 64); return n }

// logTracker procesa el log línea a línea y recuerda hasta dónde leyó, para no releer
// un log de decenas de MB cada pocos segundos.
type logTracker struct {
	off     int64
	partial string
	sum     Summary
}

func (t *logTracker) line(l string) {
	s := &t.sum
	if m := copiedRe.FindStringSubmatch(l); m != nil {
		s.Transferred++
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			s.Bytes = int64(v * units[m[2]])
		}
	}
	if m := etaRe.FindStringSubmatch(l); m != nil {
		s.ETA, s.Total = atoi(m[1]), atoi(m[3])
		s.Processed = s.Total - atoi(m[2])
		return
	}
	if m := nbMessagesRe.FindStringSubmatch(l); m != nil && s.Total == 0 {
		s.Total = atoi(m[1])
	} else if m := transferredRe.FindStringSubmatch(l); m != nil {
		s.Done, s.Transferred, s.ETA = true, atoi(m[1]), 0
		if s.Total > 0 {
			s.Processed = s.Total
		}
	} else if m := bytesRe.FindStringSubmatch(l); m != nil {
		s.Bytes = atoi(m[1])
	} else if m := errorsRe.FindStringSubmatch(l); m != nil {
		s.Errors = atoi(m[1])
	}
}

// feed procesa texto nuevo; una línea a medio escribir queda pendiente para la próxima vez.
func (t *logTracker) feed(b string) {
	b = t.partial + b
	lines := strings.Split(b, "\n")
	t.partial = lines[len(lines)-1]
	for _, l := range lines[:len(lines)-1] {
		t.line(strings.TrimSuffix(l, "\r"))
	}
}

func (t *logTracker) read(f *os.File) {
	if _, err := f.Seek(t.off, io.SeekStart); err != nil {
		return
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return
	}
	t.off += int64(len(b))
	t.feed(string(b))
}

// ParseImportLog procesa un log completo (o un trozo) de imapsync.
func ParseImportLog(s string) Summary {
	var t logTracker
	t.feed(s + "\n")
	return t.sum
}

// exitMessage traduce los códigos de salida de imapsync.
func exitMessage(code int, errors int64) string {
	switch code {
	case 10, 101:
		return "No se pudo conectar al servidor de origen. Revisa el servidor y el puerto."
	case 102:
		return "No se pudo conectar a Stalwart."
	case 12:
		return "Falló la conexión segura (TLS). Prueba con el otro puerto (993 o 143)."
	case 16, 161:
		return "Usuario o contraseña de origen incorrectos."
	case 162:
		return "Stalwart rechazó la credencial de administrador."
	case 113:
		return "El buzón de destino se quedó sin espacio: sube la cuota y sincroniza de nuevo."
	case 111, 112, 114, 115, 116, 117, 120, 121:
		return fmt.Sprintf("Terminó con %d errores en algunos correos o carpetas. Revisa el log y sincroniza de nuevo.", errors)
	case 6, 137, 143:
		return "Se detuvo antes de terminar. Sincroniza de nuevo para retomar."
	}
	return fmt.Sprintf("imapsync terminó con código %d. Revisa el log.", code)
}
