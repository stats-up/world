// Package mail administra el servidor de correo Stalwart: un solo contenedor
// multi-dominio con SMTP (25/465/587) e IMAPS (993) publicados en el host,
// y su HTTP interno (JMAP, OAuth, administración) solo en la red Docker de world.
package mail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"world/internal/config"
	"world/internal/docker"
	"world/internal/proxy"
)

const (
	ContainerName = "world-stalwart"
	// Versión fija: Stalwart se actualiza a mano después de leer el changelog (no como world).
	Image      = "stalwartlabs/stalwart:v0.16.25"
	DataVolume = "world-stalwart-data"
	AdminUser  = "admin"
	// HTTP interno de Stalwart (JMAP, OAuth, API de administración). No se publica en el host.
	InternalURL = "http://" + ContainerName + ":8080"

	memoryLimit = 1 << 30 // 1 GB
	stalwartUID = 2000    // usuario "stalwart" de la imagen oficial
	hashLabel   = "world.config-hash"
)

// Puertos publicados en el host. 143, 110/995 y 4190 quedan cerrados a propósito.
var publishedPorts = []string{"25", "465", "587", "993"}

// Settings es la configuración de correo que se guarda en los ajustes de world (secretos cifrados).
type Settings struct {
	Enabled     bool
	Hostname    string // nombre del servidor (PTR, saludo SMTP), ej: mx.statsup.cl
	S3Bucket    string
	S3Prefix    string
	S3AccessKey string
	S3SecretKey string
	CFToken     string // token de Cloudflare para los certificados (DNS-01)
	AdminSecret string // contraseña del administrador de Stalwart (la elige el usuario)
}

// Missing lista lo que falta para poder activar el correo.
func (s Settings) Missing() []string {
	var out []string
	if s.Hostname == "" {
		out = append(out, "nombre del servidor")
	}
	if s.S3Bucket == "" || s.S3AccessKey == "" || s.S3SecretKey == "" {
		out = append(out, "bucket y llaves de acceso")
	}
	if s.CFToken == "" {
		out = append(out, "token de Cloudflare")
	}
	if s.AdminSecret == "" {
		out = append(out, "contraseña de administrador")
	}
	return out
}

type Manager struct {
	dc  *docker.Client
	cfg config.Config
}

func New(dc *docker.Client, cfg config.Config) *Manager {
	return &Manager{dc: dc, cfg: cfg}
}

// env arma las variables del contenedor. Los secretos van como variables de ambiente:
// Stalwart los lee con {"@type":"EnvironmentVariable"} sin guardarlos en su base de datos.
func (s Settings) env() []string {
	env := []string{
		"STALWART_HOSTNAME=" + s.Hostname,
		// Credencial de administración que usa world contra la API interna (puerto 8080).
		"STALWART_RECOVERY_ADMIN=" + AdminUser + ":" + s.AdminSecret,
		"S3_ACCESS_KEY=" + s.S3AccessKey,
		"S3_SECRET_KEY=" + s.S3SecretKey,
		"CF_API_TOKEN=" + s.CFToken,
	}
	sort.Strings(env)
	return env
}

// dataStoreConfig es el único archivo de configuración de Stalwart v0.16: el resto vive en su base de datos.
// Cachés de RocksDB reducidas (por defecto 128 MB cada una) para quedar holgados bajo el límite de 1 GB.
const dataStoreConfig = `{"@type":"RocksDb","path":"/var/lib/stalwart/","cacheSize":67108864,"bufferSize":33554432}
`

// Ensure deja Stalwart corriendo con la configuración pedida, o lo quita si el correo está desactivado.
// Los datos (volumen) nunca se borran desde aquí.
func (m *Manager) Ensure(ctx context.Context, s Settings) error {
	if !s.Enabled {
		return m.remove(ctx)
	}
	if missing := s.Missing(); len(missing) > 0 {
		return fmt.Errorf("falta: %s", strings.Join(missing, ", "))
	}
	if err := m.dc.NetworkEnsure(ctx, proxy.Network); err != nil {
		return fmt.Errorf("creando red %s: %w", proxy.Network, err)
	}
	etcDir, logDir := m.cfg.Path("stalwart", "etc"), m.cfg.Path("stalwart", "logs")
	if err := writeOwned(filepath.Join(etcDir, "config.json"), dataStoreConfig); err != nil {
		return err
	}
	// El tracer por defecto de Stalwart escribe en /var/log/stalwart, que la imagen no trae.
	if err := mkdirOwned(logDir); err != nil {
		return err
	}

	env := s.env()
	sum := sha256.Sum256([]byte(Image + "\n" + dataStoreConfig + strings.Join(env, "\n")))
	hash := hex.EncodeToString(sum[:8])

	info, err := m.dc.ContainerInspect(ctx, ContainerName)
	switch {
	case err == nil && info.Config.Labels[hashLabel] == hash:
		if !info.State.Running {
			return m.dc.ContainerStart(ctx, info.ID)
		}
		return nil
	case err == nil:
		log.Printf("stalwart: configuración cambió, recreando contenedor")
		if err := m.dc.ContainerStop(ctx, info.ID, 30*time.Second); err != nil && !docker.IsNotFound(err) {
			log.Printf("stalwart: deteniendo: %v", err)
		}
		if err := m.dc.ContainerRemove(ctx, info.ID); err != nil {
			return err
		}
	case !docker.IsNotFound(err):
		return err
	}

	if err := m.dc.ImagePull(ctx, Image); err != nil {
		return err
	}
	exposed := map[string]struct{}{"8080/tcp": {}}
	bindings := map[string][]docker.PortBinding{}
	for _, p := range publishedPorts {
		exposed[p+"/tcp"] = struct{}{}
		bindings[p+"/tcp"] = []docker.PortBinding{{HostPort: p}}
	}
	id, err := m.dc.ContainerCreate(ctx, ContainerName, docker.ContainerConfig{
		Image:        Image,
		Env:          env,
		Labels:       map[string]string{hashLabel: hash, "world.managed": "stalwart"},
		ExposedPorts: exposed,
		HostConfig: docker.HostConfig{
			Binds: []string{
				DataVolume + ":/var/lib/stalwart",
				etcDir + ":/etc/stalwart",
				logDir + ":/var/log/stalwart",
			},
			PortBindings:  bindings,
			RestartPolicy: docker.RestartPolicy{Name: "unless-stopped"},
			Memory:        memoryLimit,
		},
		// La red world es solo IPv4: el correo sale siempre por la IPv4, que es la que tiene PTR (mx.statsup.cl).
		// Si algún día se activa IPv6 en Docker, configurar antes el PTR de la IPv6 o Gmail rechazará los envíos.
		NetworkingConfig: &docker.NetworkingConfig{EndpointsConfig: map[string]struct{}{proxy.Network: {}}},
	})
	if err != nil {
		return fmt.Errorf("creando stalwart: %w", err)
	}
	return m.dc.ContainerStart(ctx, id)
}

func (m *Manager) remove(ctx context.Context) error {
	info, err := m.dc.ContainerInspect(ctx, ContainerName)
	if docker.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	log.Printf("stalwart: correo desactivado, quitando el contenedor (los datos se conservan)")
	if err := m.dc.ContainerStop(ctx, info.ID, 30*time.Second); err != nil && !docker.IsNotFound(err) {
		log.Printf("stalwart: deteniendo: %v", err)
	}
	return m.dc.ContainerRemove(ctx, info.ID)
}

// Status resume el estado del contenedor para la UI y /health.
type Status struct {
	State  string `json:"state"`            // no instalado | running | exited | ...
	Health string `json:"health,omitempty"` // starting | healthy | unhealthy (healthcheck de la imagen)
	OOM    bool   `json:"oom_killed,omitempty"`
}

// OK: corriendo y sin healthcheck fallido ("starting" cuenta como OK para no alertar en cada reinicio).
func (s Status) OK() bool {
	return s.State == "running" && s.Health != "unhealthy"
}

func (m *Manager) Status(ctx context.Context) Status {
	info, err := m.dc.ContainerInspect(ctx, ContainerName)
	if docker.IsNotFound(err) {
		return Status{State: "no instalado"}
	}
	if err != nil {
		return Status{State: "desconocido"}
	}
	st := Status{State: info.State.Status, OOM: info.State.OOMKilled}
	if info.State.Health != nil {
		st.Health = info.State.Health.Status
	}
	return st
}

// EnsureLoop reintenta Ensure al arrancar el panel (Docker puede tardar en estar listo tras un reinicio).
func (m *Manager) EnsureLoop(ctx context.Context, settings func() Settings) {
	delay := 2 * time.Second
	for {
		err := m.Ensure(ctx, settings())
		if err == nil {
			return
		}
		log.Printf("stalwart: %v (reintentando en %s)", err, delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if delay < 5*time.Minute {
			delay *= 2
		}
	}
}

// writeOwned escribe un archivo legible por el usuario de Stalwart (UID 2000) y nadie más.
func writeOwned(path, content string) error {
	if err := mkdirOwned(filepath.Dir(path)); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return err
	}
	return chown(path)
}

func mkdirOwned(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return chown(dir)
}

func chown(path string) error {
	err := os.Chown(path, stalwartUID, stalwartUID)
	if err != nil && os.Geteuid() != 0 {
		return nil // en desarrollo (sin root) no se puede ni hace falta
	}
	return err
}
