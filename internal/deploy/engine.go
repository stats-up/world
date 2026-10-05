// Package deploy ejecuta el ciclo de vida de los sitios: clonar, construir la imagen,
// levantar el contenedor nuevo, verificar que arranque y retirar el anterior.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"world/internal/config"
	"world/internal/docker"
	"world/internal/proxy"
	"world/internal/secret"
	"world/internal/store"
)

var ErrBusy = errors.New("ya hay un deploy en curso para este sitio")

const (
	labelSite       = "world.site"
	labelDeployment = "world.deployment"
	keepImages      = 2 // actual + anterior (para un futuro rollback)
)

type Engine struct {
	cfg config.Config
	st  *store.Store
	dc  *docker.Client
	box *secret.Box

	mu   sync.Mutex
	busy map[int64]bool
}

func New(cfg config.Config, st *store.Store, dc *docker.Client, box *secret.Box) *Engine {
	return &Engine{cfg: cfg, st: st, dc: dc, box: box, busy: map[int64]bool{}}
}

func (e *Engine) LogPath(deployID int64) string {
	return e.cfg.Path("deployments", fmt.Sprintf("%d.log", deployID))
}

// Start lanza un deploy en segundo plano y devuelve su id. source: store.SourceManual o store.SourceAuto.
func (e *Engine) Start(siteID int64, source string) (int64, error) {
	e.mu.Lock()
	if e.busy[siteID] {
		e.mu.Unlock()
		return 0, ErrBusy
	}
	e.busy[siteID] = true
	e.mu.Unlock()

	id, err := e.st.CreateDeployment(siteID, source)
	if err != nil {
		e.release(siteID)
		return 0, err
	}
	go e.run(id, siteID)
	return id, nil
}

func (e *Engine) release(siteID int64) {
	e.mu.Lock()
	delete(e.busy, siteID)
	e.mu.Unlock()
}

func (e *Engine) run(deployID, siteID int64) {
	defer e.release(siteID)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	if err := os.MkdirAll(filepath.Dir(e.LogPath(deployID)), 0o700); err != nil {
		e.st.FinishDeployment(deployID, store.DeployFailed, "", err.Error())
		return
	}
	f, err := os.OpenFile(e.LogPath(deployID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		e.st.FinishDeployment(deployID, store.DeployFailed, "", err.Error())
		return
	}
	defer f.Close()
	lg := &logger{w: f}

	start := time.Now()
	sha, err := e.deploy(ctx, lg, deployID, siteID)
	if err != nil {
		lg.Printf("\n✗ Deploy fallido: %v", err)
		e.st.FinishDeployment(deployID, store.DeployFailed, sha, err.Error())
		return
	}
	lg.Printf("\n✓ Deploy completado en %s", time.Since(start).Round(time.Second))
	e.st.FinishDeployment(deployID, store.DeploySuccess, sha, "")
}

func (e *Engine) deploy(ctx context.Context, lg *logger, deployID, siteID int64) (string, error) {
	site, err := e.st.Site(siteID)
	if err != nil {
		return "", err
	}

	// 1. Clonar
	work := e.cfg.Path("builds", fmt.Sprintf("%s-%d", site.Name, deployID))
	os.RemoveAll(work)
	defer os.RemoveAll(work)

	lg.Step(fmt.Sprintf("Clonando %s (rama %s)", site.RepoURL, site.Branch))
	gitEnv, err := e.gitEnv(site)
	if err != nil {
		return "", err
	}
	if err := e.exec(ctx, lg, "", gitEnv, "git", "clone", "--depth", "1", "--single-branch",
		"--branch", site.Branch, site.RepoURL, work); err != nil {
		hint := ""
		if site.UsesSSH() {
			hint = " (revisa que la deploy key esté agregada en el repo de GitHub)"
		}
		return "", fmt.Errorf("git clone falló%s: %w", hint, err)
	}
	out, _ := exec.CommandContext(ctx, "git", "-C", work, "rev-parse", "HEAD").Output()
	sha := strings.TrimSpace(string(out))
	lg.Printf("Commit: %s", sha)

	// 2. Construir la imagen
	dockerfile, err := prepareBuild(site, work, lg)
	if err != nil {
		return sha, err
	}
	image := fmt.Sprintf("world/%s:%d", site.Name, deployID)
	lg.Step("Construyendo imagen " + image)
	if err := e.exec(ctx, lg, work, []string{"DOCKER_BUILDKIT=1"}, "docker", "build",
		"--progress=plain", "-t", image, "-f", dockerfile, "."); err != nil {
		return sha, fmt.Errorf("docker build falló: %w", err)
	}
	os.RemoveAll(work)

	// 3. Levantar el contenedor nuevo (el anterior sigue atendiendo mientras tanto)
	env, err := e.containerEnv(site)
	if err != nil {
		return sha, err
	}
	lg.Step("Iniciando contenedor")
	name := fmt.Sprintf("world-%s-%d", site.Name, deployID)
	id, err := e.dc.ContainerCreate(ctx, name, containerConfig(site, image, env, deployID))
	if err != nil {
		return sha, err
	}
	if err := e.dc.ContainerStart(ctx, id); err != nil {
		e.dc.ContainerRemove(ctx, id)
		return sha, err
	}
	if err := e.waitHealthy(ctx, lg, id); err != nil {
		if logs, lerr := e.dc.ContainerLogs(ctx, id, 80); lerr == nil {
			lg.Printf("\n--- Últimas líneas del contenedor ---\n%s", logs)
		}
		e.dc.ContainerRemove(ctx, id)
		return sha, err
	}

	// 4. Retirar la versión anterior
	lg.Step("Retirando versión anterior")
	olds, err := e.dc.ContainersByLabel(ctx, labelSite+"="+site.Name)
	if err != nil {
		return sha, err
	}
	for _, c := range olds {
		if c.ID == id {
			continue
		}
		e.dc.ContainerStop(ctx, c.ID, 15*time.Second)
		if err := e.dc.ContainerRemove(ctx, c.ID); err != nil {
			lg.Printf("No se pudo eliminar %s: %v", c.ID[:12], err)
			continue
		}
		lg.Printf("Eliminado %s", strings.Join(c.Names, ","))
	}
	if err := e.st.SetSiteImage(site.ID, image); err != nil {
		return sha, err
	}
	e.pruneImages(ctx, lg, site.Name)
	return sha, nil
}

func containerConfig(site *store.Site, image string, env []string, deployID int64) docker.ContainerConfig {
	labels := map[string]string{
		labelSite:       site.Name,
		labelDeployment: strconv.FormatInt(deployID, 10),
	}
	for k, v := range TraefikLabels(site) {
		labels[k] = v
	}
	cfg := docker.ContainerConfig{
		Image:        image,
		Env:          env,
		Labels:       labels,
		ExposedPorts: map[string]struct{}{fmt.Sprintf("%d/tcp", site.InternalPort()): {}},
		HostConfig: docker.HostConfig{
			RestartPolicy: docker.RestartPolicy{Name: "unless-stopped"},
			Memory:        int64(site.MemoryMB) << 20,
			NanoCPUs:      int64(site.CPUs * 1e9),
		},
		NetworkingConfig: &docker.NetworkingConfig{EndpointsConfig: map[string]struct{}{proxy.Network: {}}},
	}
	return cfg
}

// TraefikLabels genera las labels que Traefik lee para enrutar los dominios del sitio con HTTPS.
func TraefikLabels(site *store.Site) map[string]string {
	if len(site.Domains) == 0 {
		return nil
	}
	r := "site-" + site.Name
	rules := make([]string, len(site.Domains))
	for i, d := range site.Domains {
		rules[i] = "Host(`" + d + "`)"
	}
	return map[string]string{
		"traefik.enable":                                           "true",
		"traefik.docker.network":                                   proxy.Network,
		"traefik.http.routers." + r + ".rule":                      strings.Join(rules, " || "),
		"traefik.http.routers." + r + ".entrypoints":               "websecure",
		"traefik.http.routers." + r + ".tls.certresolver":          proxy.CertResolver,
		"traefik.http.routers." + r + ".service":                   r,
		"traefik.http.services." + r + ".loadbalancer.server.port": strconv.Itoa(site.InternalPort()),
	}
}

func (e *Engine) containerEnv(site *store.Site) ([]string, error) {
	var defaults []string
	if site.Kind == store.KindPHP {
		defaults = append(defaults, "PHP_OPCACHE_ENABLE=1", "LOG_CHANNEL=stderr")
		if site.Autorun {
			defaults = append(defaults, "AUTORUN_ENABLED=true")
		}
	}
	plain, err := e.box.OpenString(site.EnvEnc)
	if err != nil {
		return nil, fmt.Errorf("descifrando variables de ambiente: %w", err)
	}
	vars, err := ParseEnv(plain)
	if err != nil {
		return nil, err
	}
	return mergeEnv(defaults, vars), nil
}

// waitHealthy espera a que el contenedor quede estable (o "healthy" si la imagen define health check).
func (e *Engine) waitHealthy(ctx context.Context, lg *logger, id string) error {
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		info, err := e.dc.ContainerInspect(ctx, id)
		if err != nil {
			return err
		}
		st := info.State
		if !st.Running || st.Restarting || info.RestartCount > 0 {
			reason := fmt.Sprintf("código de salida %d", st.ExitCode)
			if st.OOMKilled {
				reason = "sin memoria (OOM): sube el límite de RAM del sitio"
			}
			return fmt.Errorf("el contenedor se detuvo al arrancar: %s", reason)
		}
		if st.Health != nil {
			switch st.Health.Status {
			case "healthy":
				lg.Printf("Health check OK")
				return nil
			case "unhealthy":
				return errors.New("el health check del contenedor falló")
			}
		} else if time.Since(info.StartedAt()) > 8*time.Second {
			lg.Printf("Contenedor estable")
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return errors.New("el contenedor no quedó saludable en 2 minutos")
}

func (e *Engine) pruneImages(ctx context.Context, lg *logger, siteName string) {
	imgs, err := e.dc.ImagesByReference(ctx, "world/"+siteName)
	if err != nil {
		return
	}
	sort.Slice(imgs, func(i, j int) bool { return imgs[i].Created > imgs[j].Created })
	for i, img := range imgs {
		if i < keepImages {
			continue
		}
		for _, tag := range img.RepoTags {
			if err := e.dc.ImageRemove(ctx, tag); err == nil {
				lg.Printf("Imagen antigua eliminada: %s", tag)
			}
		}
	}
}

func (e *Engine) writeDeployKey(site *store.Site) (string, error) {
	key, err := e.box.Open(site.DeployKeyEnc)
	if err != nil || len(key) == 0 {
		return "", errors.New("el sitio no tiene una deploy key válida")
	}
	dir := e.cfg.Path("sites", site.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "deploy_key")
	return path, os.WriteFile(path, key, 0o600)
}

// gitEnv arma el entorno de git para el sitio: sin prompts y, si el repo es privado, con su deploy key.
func (e *Engine) gitEnv(site *store.Site) ([]string, error) {
	env := []string{"GIT_TERMINAL_PROMPT=0"}
	if !site.UsesSSH() {
		return env, nil
	}
	keyPath, err := e.writeDeployKey(site)
	if err != nil {
		return nil, err
	}
	return append(env, "GIT_SSH_COMMAND=ssh -i "+shellQuote(keyPath)+
		" -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile="+shellQuote(e.cfg.Path("known_hosts"))), nil
}

// RemoteHead consulta el último commit de la rama del sitio sin descargar el repo (git ls-remote).
func (e *Engine) RemoteHead(ctx context.Context, site *store.Site) (string, error) {
	env, err := e.gitEnv(site)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--heads", site.RepoURL, "refs/heads/"+site.Branch)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 300 {
			msg = msg[len(msg)-300:]
		}
		return "", fmt.Errorf("git ls-remote: %v: %s", err, msg)
	}
	sha, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\t")
	if len(sha) < 40 {
		return "", fmt.Errorf("la rama %q no existe en el repositorio", site.Branch)
	}
	return sha, nil
}

// shellQuote: GIT_SSH_COMMAND lo interpreta una shell, así que las rutas van entre comillas simples.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (e *Engine) exec(ctx context.Context, lg *logger, dir string, env []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = lg
	cmd.Stderr = lg
	return cmd.Run()
}

// --- Control de sitios ---

// Containers devuelve los contenedores de cada sitio, indexados por nombre de sitio.
// Si un sitio tiene varios (en medio de un deploy), queda primero el que está corriendo.
func (e *Engine) Containers(ctx context.Context) (map[string]docker.ContainerSummary, error) {
	list, err := e.dc.ContainersByLabel(ctx, labelSite)
	if err != nil {
		return nil, err
	}
	out := map[string]docker.ContainerSummary{}
	for _, c := range list {
		name := c.Labels[labelSite]
		if prev, ok := out[name]; ok && prev.State == "running" {
			continue
		}
		out[name] = c
	}
	return out, nil
}

// Control ejecuta start/stop/restart sobre los contenedores del sitio.
func (e *Engine) Control(ctx context.Context, site *store.Site, action string) error {
	list, err := e.dc.ContainersByLabel(ctx, labelSite+"="+site.Name)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		return errors.New("el sitio no tiene contenedor: haz un deploy primero")
	}
	for _, c := range list {
		switch action {
		case "start":
			err = e.dc.ContainerStart(ctx, c.ID)
		case "stop":
			err = e.dc.ContainerStop(ctx, c.ID, 15*time.Second)
		case "restart":
			err = e.dc.ContainerRestart(ctx, c.ID, 15*time.Second)
		default:
			return fmt.Errorf("acción desconocida: %s", action)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) Logs(ctx context.Context, site *store.Site, tail int) (string, error) {
	cs, err := e.Containers(ctx)
	if err != nil {
		return "", err
	}
	c, ok := cs[site.Name]
	if !ok {
		return "", errors.New("el sitio no tiene contenedor")
	}
	return e.dc.ContainerLogs(ctx, c.ID, tail)
}

// Remove elimina contenedores, imágenes y archivos del sitio (no toca la base de datos del panel).
func (e *Engine) Remove(ctx context.Context, site *store.Site) error {
	list, err := e.dc.ContainersByLabel(ctx, labelSite+"="+site.Name)
	if err != nil {
		return err
	}
	for _, c := range list {
		if err := e.dc.ContainerRemove(ctx, c.ID); err != nil {
			return err
		}
	}
	if imgs, err := e.dc.ImagesByReference(ctx, "world/"+site.Name); err == nil {
		for _, img := range imgs {
			for _, tag := range img.RepoTags {
				e.dc.ImageRemove(ctx, tag)
			}
		}
	}
	return os.RemoveAll(e.cfg.Path("sites", site.Name))
}

type logger struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *logger) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func (l *logger) Printf(format string, a ...any) { fmt.Fprintf(l, format+"\n", a...) }

func (l *logger) Step(s string) { l.Printf("\n==> %s", s) }
