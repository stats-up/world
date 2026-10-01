// Package config carga la configuración del panel desde variables de ambiente
// (en producción vienen de /etc/world/world.env vía systemd).
package config

import (
	"net"
	"os"
	"path/filepath"
)

type Config struct {
	Listen       string // dirección donde escucha el panel, ej: ":9000"
	DataDir      string // SQLite, llaves, logs de deploy, config de Traefik
	DockerSocket string
	RepoDir      string // dónde está clonado este repo (lo usa el auto-update)
}

func Load() Config {
	return Config{
		Listen:       env("WORLD_LISTEN", ":9000"),
		DataDir:      env("WORLD_DATA_DIR", "/var/lib/world"),
		DockerSocket: env("WORLD_DOCKER_SOCKET", "/var/run/docker.sock"),
		RepoDir:      env("WORLD_REPO_DIR", "/opt/world"),
	}
}

// Path arma una ruta dentro del directorio de datos.
func (c Config) Path(parts ...string) string {
	return filepath.Join(append([]string{c.DataDir}, parts...)...)
}

// Port devuelve solo el puerto de Listen (lo usa Traefik para llegar al panel).
func (c Config) Port() string {
	_, port, err := net.SplitHostPort(c.Listen)
	if err != nil || port == "" {
		return "9000"
	}
	return port
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
