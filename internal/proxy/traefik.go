// Package proxy administra el contenedor de Traefik: recibe el tráfico 80/443,
// enruta cada dominio a su contenedor (vía labels) y emite certificados SSL.
package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"world/internal/config"
	"world/internal/docker"
)

const (
	Network       = "world"
	ContainerName = "world-traefik"
	Image         = "traefik:v3.6"
	CertResolver  = "le"
	hashLabel     = "world.config-hash"
)

type Manager struct {
	dc  *docker.Client
	cfg config.Config
}

func New(dc *docker.Client, cfg config.Config) *Manager {
	return &Manager{dc: dc, cfg: cfg}
}

// Ensure deja Traefik corriendo con la configuración pedida. Si ya está igual, no hace nada.
// panelDomain puede ser vacío (el panel solo queda accesible por IP:puerto).
func (m *Manager) Ensure(ctx context.Context, acmeEmail, panelDomain string) error {
	if err := m.dc.NetworkEnsure(ctx, Network); err != nil {
		return fmt.Errorf("creando red %s: %w", Network, err)
	}
	dynDir := m.cfg.Path("traefik", "dynamic")
	if err := os.MkdirAll(dynDir, 0o755); err != nil {
		return err
	}
	acme := m.cfg.Path("traefik", "acme.json")
	if _, err := os.Stat(acme); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(acme, nil, 0o600); err != nil {
			return err
		}
	}
	if err := m.writePanelRoute(dynDir, panelDomain); err != nil {
		return err
	}

	args := []string{
		"--providers.docker=true",
		"--providers.docker.exposedbydefault=false",
		"--providers.docker.network=" + Network,
		"--providers.file.directory=/etc/traefik/dynamic",
		"--providers.file.watch=true",
		"--entrypoints.web.address=:80",
		"--entrypoints.websecure.address=:443",
		"--entrypoints.web.http.redirections.entrypoint.to=websecure",
		"--entrypoints.web.http.redirections.entrypoint.scheme=https",
		"--certificatesresolvers." + CertResolver + ".acme.storage=/letsencrypt/acme.json",
		"--certificatesresolvers." + CertResolver + ".acme.httpchallenge.entrypoint=web",
		"--log.level=INFO",
	}
	if acmeEmail != "" {
		args = append(args, "--certificatesresolvers."+CertResolver+".acme.email="+acmeEmail)
	}
	sum := sha256.Sum256([]byte(Image + "\n" + strings.Join(args, "\n")))
	hash := hex.EncodeToString(sum[:8])

	info, err := m.dc.ContainerInspect(ctx, ContainerName)
	switch {
	case err == nil && info.Config.Labels[hashLabel] == hash:
		if !info.State.Running {
			return m.dc.ContainerStart(ctx, info.ID)
		}
		return nil
	case err == nil:
		log.Printf("traefik: configuración cambió, recreando contenedor")
		if err := m.dc.ContainerRemove(ctx, info.ID); err != nil {
			return err
		}
	case !docker.IsNotFound(err):
		return err
	}

	if err := m.dc.ImagePull(ctx, Image); err != nil {
		return err
	}
	id, err := m.dc.ContainerCreate(ctx, ContainerName, docker.ContainerConfig{
		Image:        Image,
		Cmd:          args,
		Labels:       map[string]string{hashLabel: hash, "world.managed": "traefik"},
		ExposedPorts: map[string]struct{}{"80/tcp": {}, "443/tcp": {}},
		HostConfig: docker.HostConfig{
			Binds: []string{
				"/var/run/docker.sock:/var/run/docker.sock:ro",
				dynDir + ":/etc/traefik/dynamic:ro",
				acme + ":/letsencrypt/acme.json",
			},
			PortBindings: map[string][]docker.PortBinding{
				"80/tcp":  {{HostPort: "80"}},
				"443/tcp": {{HostPort: "443"}},
			},
			RestartPolicy: docker.RestartPolicy{Name: "unless-stopped"},
			// Para llegar al panel, que corre en el host (fuera de Docker).
			ExtraHosts: []string{"host.docker.internal:host-gateway"},
		},
		NetworkingConfig: &docker.NetworkingConfig{EndpointsConfig: map[string]struct{}{Network: {}}},
	})
	if err != nil {
		return fmt.Errorf("creando traefik: %w", err)
	}
	return m.dc.ContainerStart(ctx, id)
}

// writePanelRoute publica el panel en su dominio con HTTPS (Traefik → host:puerto del panel).
func (m *Manager) writePanelRoute(dir, domain string) error {
	path := filepath.Join(dir, "panel.yml")
	if domain == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	yml := fmt.Sprintf(`# Generado por world: no editar a mano.
http:
  routers:
    world-panel:
      rule: "Host(%s)"
      entryPoints: [websecure]
      service: world-panel
      tls:
        certResolver: %s
  services:
    world-panel:
      loadBalancer:
        servers:
          - url: "http://host.docker.internal:%s"
`, "`"+domain+"`", CertResolver, m.cfg.Port())
	return os.WriteFile(path, []byte(yml), 0o644)
}

// Status resume el estado del contenedor de Traefik para mostrarlo en la UI.
func (m *Manager) Status(ctx context.Context) string {
	info, err := m.dc.ContainerInspect(ctx, ContainerName)
	if docker.IsNotFound(err) {
		return "no instalado"
	}
	if err != nil {
		return "desconocido"
	}
	return info.State.Status
}

// EnsureLoop reintenta Ensure al arrancar el panel (Docker puede tardar en estar listo tras un reinicio).
func (m *Manager) EnsureLoop(ctx context.Context, settings func() (email, domain string)) {
	delay := 2 * time.Second
	for {
		email, domain := settings()
		err := m.Ensure(ctx, email, domain)
		if err == nil {
			log.Printf("traefik: listo")
			return
		}
		log.Printf("traefik: %v (reintentando en %s)", err, delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if delay < time.Minute {
			delay *= 2
		}
	}
}
