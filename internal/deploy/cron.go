package deploy

import (
	"context"
	"errors"
	"log"
	"strconv"
	"sync"
	"time"

	"world/internal/store"
)

// Cron ejecuta cada minuto el comando cron de cada sitio dentro de su contenedor
// (ej: "php artisan schedule:run", que a su vez dispara las tareas del scheduler de Laravel).
type Cron struct {
	engine   *Engine
	st       *store.Store
	Interval time.Duration
	Timeout  time.Duration // tiempo máximo de una ejecución

	mu      sync.Mutex
	running map[int64]bool // sin solapamiento: si la anterior no terminó, se salta el minuto
	started time.Time
	lastRun time.Time
	lastErr string
}

// cronOutputMax: se guarda solo el final de la salida de la última ejecución.
const cronOutputMax = 4000

func NewCron(engine *Engine, st *store.Store, interval time.Duration) *Cron {
	return &Cron{engine: engine, st: st, Interval: interval, Timeout: 15 * time.Minute, running: map[int64]bool{}}
}

// Run espera al inicio del próximo minuto (schedule:run evalúa las tareas del minuto en curso)
// y luego ejecuta un ciclo por intervalo hasta que ctx se cancele.
func (c *Cron) Run(ctx context.Context) {
	c.mu.Lock()
	c.started = time.Now()
	c.mu.Unlock()
	wait := time.Until(time.Now().Truncate(c.Interval).Add(c.Interval))
	select {
	case <-ctx.Done():
		return
	case <-time.After(wait):
	}
	t := time.NewTicker(c.Interval)
	defer t.Stop()
	for {
		c.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *Cron) Health() Health {
	c.mu.Lock()
	defer c.mu.Unlock()
	ref := c.lastRun
	if ref.IsZero() {
		ref = c.started // recién iniciado: espera el comienzo del minuto, no es una falla
	}
	return Health{
		LastRun: c.lastRun,
		OK:      !ref.IsZero() && time.Since(ref) < c.Interval*5/2,
		Error:   c.lastErr,
	}
}

func (c *Cron) tick(ctx context.Context) {
	sites, err := c.st.Sites()
	c.mu.Lock()
	c.lastRun = time.Now()
	c.lastErr = ""
	if err != nil {
		c.lastErr = err.Error()
	}
	c.mu.Unlock()
	if err != nil {
		log.Printf("cron: %v", err)
		return
	}
	for _, site := range sites {
		if site.CronCommand == "" {
			continue
		}
		c.mu.Lock()
		busy := c.running[site.ID]
		if !busy {
			c.running[site.ID] = true
		}
		c.mu.Unlock()
		if busy {
			continue
		}
		go func(site *store.Site) {
			defer func() {
				c.mu.Lock()
				delete(c.running, site.ID)
				c.mu.Unlock()
			}()
			c.runSite(ctx, site)
		}(site)
	}
}

func (c *Cron) runSite(ctx context.Context, site *store.Site) {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	start := time.Now()
	id, err := c.engine.RunningContainer(ctx, site)
	if err != nil {
		// Sin contenedor (aún no desplegado o detenido): no hay nada que ejecutar.
		return
	}
	out, code, err := c.engine.dc.ContainerExec(ctx, id, []string{"sh", "-c", site.CronCommand})
	if err != nil {
		out += "\n[world] " + err.Error()
		if code == 0 {
			code = -1
		}
	}
	if len(out) > cronOutputMax {
		out = "…" + out[len(out)-cronOutputMax:]
	}
	c.st.SetSiteCron(site.ID, start, code, time.Since(start), out)
	if code != 0 {
		log.Printf("cron %s: salió con código %d", site.Name, code)
	}
}

// RunningContainer devuelve el contenedor activo del sitio. Durante un deploy conviven dos:
// se prefiere el de la versión vigente (la que ya pasó la verificación), si no, el más reciente.
func (e *Engine) RunningContainer(ctx context.Context, site *store.Site) (string, error) {
	list, err := e.dc.ContainersByLabel(ctx, labelSite+"="+site.Name)
	if err != nil {
		return "", err
	}
	best, bestDeploy := "", int64(-1)
	for _, ct := range list {
		if ct.State != "running" {
			continue
		}
		if site.CurrentImage != "" && ct.Image == site.CurrentImage {
			return ct.ID, nil
		}
		d, _ := strconv.ParseInt(ct.Labels[labelDeployment], 10, 64)
		if d > bestDeploy {
			best, bestDeploy = ct.ID, d
		}
	}
	if best == "" {
		return "", errors.New("el sitio no tiene un contenedor corriendo")
	}
	return best, nil
}
