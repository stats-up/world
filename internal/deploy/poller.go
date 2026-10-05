package deploy

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"world/internal/store"
)

// Poller revisa periódicamente la rama de cada sitio con auto-deploy activado y,
// si apareció un commit nuevo, lanza el deploy. Reemplaza el "git pull" manual.
type Poller struct {
	engine   *Engine
	st       *store.Store
	Interval time.Duration

	mu      sync.Mutex
	lastRun time.Time
	lastErr string
}

// Si un deploy falla antes de saber su commit (ej: deploy key sin agregar), se reintenta como máximo cada esto.
const failedRetryAfter = 10 * time.Minute

func NewPoller(engine *Engine, st *store.Store, interval time.Duration) *Poller {
	return &Poller{engine: engine, st: st, Interval: interval}
}

// Run ejecuta un ciclo al iniciar y luego uno cada Interval, hasta que ctx se cancele.
func (p *Poller) Run(ctx context.Context) {
	t := time.NewTicker(p.Interval)
	defer t.Stop()
	for {
		p.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Health resume el estado del ciclo para la UI y el endpoint /health.
type Health struct {
	LastRun time.Time
	OK      bool   // el último ciclo fue hace menos de 2,5 intervalos
	Error   string // error general del último ciclo (no de un sitio en particular)
}

func (p *Poller) Health() Health {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Health{
		LastRun: p.lastRun,
		OK:      !p.lastRun.IsZero() && time.Since(p.lastRun) < p.Interval*5/2,
		Error:   p.lastErr,
	}
}

func (p *Poller) tick(ctx context.Context) {
	errMsg := ""
	defer func() {
		p.mu.Lock()
		p.lastRun = time.Now()
		p.lastErr = errMsg
		p.mu.Unlock()
	}()

	sites, err := p.st.Sites()
	if err != nil {
		errMsg = err.Error()
		log.Printf("auto-deploy: %v", err)
		return
	}
	for _, site := range sites {
		if !site.AutoDeploy {
			continue
		}
		p.check(ctx, site)
	}
}

func (p *Poller) check(ctx context.Context, site *store.Site) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	sha, err := p.engine.RemoteHead(cctx, site)
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	p.st.SetSiteCheck(site.ID, time.Now(), sha, errMsg)
	if err != nil {
		return
	}

	if !shouldDeploy(p.st, site.ID, sha) {
		return
	}
	id, err := p.engine.Start(site.ID, store.SourceAuto)
	switch {
	case errors.Is(err, ErrBusy):
		// ya hay un deploy en curso: se reevalúa en el próximo ciclo
	case err != nil:
		log.Printf("auto-deploy %s: %v", site.Name, err)
	default:
		log.Printf("auto-deploy %s: commit nuevo %s, deploy #%d", site.Name, sha[:12], id)
	}
}

// shouldDeploy decide si el commit remoto requiere un deploy nuevo.
func shouldDeploy(st *store.Store, siteID int64, remote string) bool {
	last, err := st.LatestDeployment(siteID)
	if errors.Is(err, store.ErrNotFound) {
		return true // nunca desplegado
	}
	if err != nil || last.Running() {
		return false
	}
	// Ese commit ya se desplegó, o ya falló: no se reintenta en bucle cada minuto.
	if last.IsCommit(remote) {
		return false
	}
	// Falló antes de clonar (sin commit conocido): esperar antes de reintentar.
	if last.Status == store.DeployFailed && last.CommitSHA == "" && time.Since(last.FinishedAt) < failedRetryAfter {
		return false
	}
	return true
}
