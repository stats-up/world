// Package phpcheck compara la versión de PHP de cada sitio con la última publicada en php.net, para
// avisar cuando sale un parche de seguridad y saber si la imagen base (serversideup/php) ya lo trae.
package phpcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"world/internal/docker"
	"world/internal/store"
)

const releasesURL = "https://www.php.net/releases/active.php"

// Status es la situación de PHP de un sitio.
type Status struct {
	Branch    string // 8.3
	Running   string // versión en el contenedor actual (vacío si no se pudo leer)
	Latest    string // última versión publicada de la rama
	Image     string // versión que trae hoy la imagen base
	Supported bool   // la rama recibe parches de seguridad
	Security  bool   // la última versión de la rama es un parche de seguridad
	Known     bool   // se pudo leer la lista de php.net (si no, Supported no es confiable)
}

// Outdated: el contenedor corre una versión anterior a la última publicada.
func (s Status) Outdated() bool { return s.Running != "" && s.Latest != "" && Less(s.Running, s.Latest) }

// ImageReady: la imagen base ya trae la última versión (basta un deploy para actualizar).
func (s Status) ImageReady() bool { return s.Image != "" && s.Latest != "" && !Less(s.Image, s.Latest) }

// NeedsDeploy: el sitio quedaría al día con un deploy (la imagen base es más nueva que el contenedor).
func (s Status) NeedsDeploy() bool { return s.Running != "" && s.Image != "" && Less(s.Running, s.Image) }

// Release: lo que php.net publica por rama.
type Release struct {
	Version  string
	Security bool
}

type Checker struct {
	dc       *docker.Client
	sites    func() ([]*store.Site, error)
	Interval time.Duration
	http     *http.Client

	mu        sync.Mutex
	releases  map[string]Release // por rama
	supported map[string]bool
	images    map[string]string // rama → versión de la imagen base
	running   map[string]string // sitio → versión en el contenedor
	checked   time.Time
	lastErr   string
	refresh   chan struct{}
}

func New(dc *docker.Client, sites func() ([]*store.Site, error)) *Checker {
	return &Checker{dc: dc, sites: sites, Interval: time.Hour, http: &http.Client{Timeout: 20 * time.Second},
		refresh: make(chan struct{}, 1)}
}

// Run revisa al iniciar y luego cada Interval (o antes si se pide con Refresh).
func (c *Checker) Run(ctx context.Context) {
	for {
		c.check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(c.Interval):
		case <-c.refresh:
		}
	}
}

// Refresh pide una revisión ahora (ej. después de un deploy), sin bloquear.
func (c *Checker) Refresh() {
	select {
	case c.refresh <- struct{}{}:
	default:
	}
}

// Site devuelve el estado de PHP del sitio; ok=false si no es PHP o aún no se revisa.
func (c *Checker) Site(site *store.Site) (Status, bool) {
	if site.Kind != store.KindPHP {
		return Status{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.checked.IsZero() {
		return Status{}, false
	}
	b := site.PHPVersion
	r := c.releases[b]
	return Status{Branch: b, Running: c.running[site.Name], Latest: r.Version, Image: c.images[b],
		Supported: c.supported[b], Security: r.Security, Known: len(c.releases) > 0}, true
}

// Checked: cuándo fue la última revisión y su error (si php.net no respondió).
func (c *Checker) Checked() (time.Time, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.checked, c.lastErr
}

func (c *Checker) check(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	releases, supported, err := c.fetchReleases(ctx)
	sites, serr := c.sites()
	if serr != nil {
		log.Printf("phpcheck: %v", serr)
		return
	}
	branches := map[string]bool{}
	running := map[string]string{}
	for _, s := range sites {
		if s.Kind != store.KindPHP {
			continue
		}
		branches[s.PHPVersion] = true
		if v := c.containerVersion(ctx, s.Name); v != "" {
			running[s.Name] = v
		}
	}
	// La imagen base se descarga de nuevo: así se sabe si serversideup ya publicó el parche (y el
	// próximo deploy la usa sin esperar).
	images := map[string]string{}
	for b := range branches {
		ref := "serversideup/php:" + b + "-fpm-nginx"
		if err := c.dc.ImagePull(ctx, ref); err != nil {
			log.Printf("phpcheck: %v", err)
		}
		if env, err := c.dc.ImageEnv(ctx, ref); err == nil {
			images[b] = envValue(env, "PHP_VERSION")
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		c.releases, c.supported, c.lastErr = releases, supported, ""
	} else {
		c.lastErr = err.Error()
		log.Printf("phpcheck: %v", err)
	}
	c.images, c.running, c.checked = images, running, time.Now()
}

// containerVersion lee PHP_VERSION del contenedor corriendo del sitio (viene de la imagen base).
func (c *Checker) containerVersion(ctx context.Context, site string) string {
	list, err := c.dc.ContainersByLabel(ctx, "world.site="+site)
	if err != nil {
		return ""
	}
	for _, ct := range list {
		if ct.State != "running" {
			continue
		}
		if info, err := c.dc.ContainerInspect(ctx, ct.ID); err == nil {
			if v := envValue(info.Config.Env, "PHP_VERSION"); v != "" {
				return v
			}
		}
	}
	return ""
}

func (c *Checker) fetchReleases(ctx context.Context) (map[string]Release, map[string]bool, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", releasesURL, nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("consultando php.net: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("php.net respondió %s", resp.Status)
	}
	return parseReleases(resp.Body)
}

// parseReleases lee releases/active.php: {"8": {"8.3": {"version": "8.3.36", "tags": ["security"], …}}}.
// Las ramas que aparecen ahí son las que tienen soporte.
func parseReleases(r interface{ Read([]byte) (int, error) }) (map[string]Release, map[string]bool, error) {
	var raw map[string]map[string]struct {
		Version string   `json:"version"`
		Tags    []string `json:"tags"`
	}
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		return nil, nil, fmt.Errorf("respuesta de php.net inválida: %w", err)
	}
	rel, sup := map[string]Release{}, map[string]bool{}
	for _, branches := range raw {
		for b, v := range branches {
			if v.Version == "" {
				continue
			}
			sec := false
			for _, t := range v.Tags {
				sec = sec || t == "security"
			}
			rel[b], sup[b] = Release{Version: v.Version, Security: sec}, true
		}
	}
	if len(rel) == 0 {
		return nil, nil, fmt.Errorf("php.net no devolvió versiones")
	}
	return rel, sup, nil
}

func envValue(env []string, key string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// Less compara versiones "8.3.35" < "8.3.36" (número por número).
func Less(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}
