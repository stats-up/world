package web

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"time"

	"world/internal/mail"
)

// selfUpdateMaxAge: el timer de auto-update corre cada minuto; sobre esto se considera detenido.
const selfUpdateMaxAge = 3 * time.Minute

type cronStatus struct {
	LastRun    time.Time `json:"last_run"`
	SecondsAgo int       `json:"seconds_ago"`
	OK         bool      `json:"ok"`
	Error      string    `json:"error,omitempty"`
}

type healthReport struct {
	Version    string     `json:"version"`
	OK         bool       `json:"ok"`
	AutoDeploy cronStatus `json:"auto_deploy"`    // revisión de ramas de los sitios (dentro del panel)
	SelfUpdate cronStatus `json:"self_update"`    // auto-update del propio panel (timer de systemd)
	SiteCron   cronStatus `json:"site_cron"`      // cron de los sitios (ej: schedule:run), dentro del panel
	Mail       *mailState `json:"mail,omitempty"` // solo si el correo está activado
}

type mailState struct {
	mail.Status
	OK bool `json:"ok"`
}

func newCronStatus(last time.Time, ok bool, errMsg string) cronStatus {
	c := cronStatus{LastRun: last, OK: ok, Error: errMsg, SecondsAgo: -1}
	if !last.IsZero() {
		c.SecondsAgo = int(time.Since(last).Seconds())
	}
	return c
}

func (s *Server) healthStatus(ctx context.Context) healthReport {
	ph := s.poller.Health()
	rep := healthReport{
		Version:    s.version,
		AutoDeploy: newCronStatus(ph.LastRun, ph.OK, ph.Error),
	}
	ch := s.cron.Health()
	rep.SiteCron = newCronStatus(ch.LastRun, ch.OK, ch.Error)
	// update.sh deja la hora de su último chequeo en este archivo (ver scripts/update.sh).
	if st, err := os.Stat(s.cfg.Path("update-heartbeat")); err == nil {
		rep.SelfUpdate = newCronStatus(st.ModTime(), time.Since(st.ModTime()) < selfUpdateMaxAge, "")
	} else {
		rep.SelfUpdate = newCronStatus(time.Time{}, false, "sin registro: el timer world-update no ha corrido")
	}
	rep.OK = rep.AutoDeploy.OK && rep.SelfUpdate.OK && rep.SiteCron.OK
	if mail.Enabled(s.st) {
		st := s.mail.Status(ctx)
		rep.Mail = &mailState{Status: st, OK: st.OK()}
		rep.OK = rep.OK && st.OK()
	}
	return rep
}

// health es público (sin login) para monitores externos tipo UptimeRobot: responde 503 si algún cron está detenido.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	rep := s.healthStatus(r.Context())
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if !rep.OK {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	json.NewEncoder(w).Encode(rep)
}
