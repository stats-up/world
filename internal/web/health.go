package web

import (
	"encoding/json"
	"net/http"
	"os"
	"time"
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
	AutoDeploy cronStatus `json:"auto_deploy"` // revisión de ramas de los sitios (dentro del panel)
	SelfUpdate cronStatus `json:"self_update"` // auto-update del propio panel (timer de systemd)
}

func newCronStatus(last time.Time, ok bool, errMsg string) cronStatus {
	c := cronStatus{LastRun: last, OK: ok, Error: errMsg, SecondsAgo: -1}
	if !last.IsZero() {
		c.SecondsAgo = int(time.Since(last).Seconds())
	}
	return c
}

func (s *Server) healthStatus() healthReport {
	ph := s.poller.Health()
	rep := healthReport{
		Version:    s.version,
		AutoDeploy: newCronStatus(ph.LastRun, ph.OK, ph.Error),
	}
	// update.sh deja la hora de su último chequeo en este archivo (ver scripts/update.sh).
	if st, err := os.Stat(s.cfg.Path("update-heartbeat")); err == nil {
		rep.SelfUpdate = newCronStatus(st.ModTime(), time.Since(st.ModTime()) < selfUpdateMaxAge, "")
	} else {
		rep.SelfUpdate = newCronStatus(time.Time{}, false, "sin registro: el timer world-update no ha corrido")
	}
	rep.OK = rep.AutoDeploy.OK && rep.SelfUpdate.OK
	return rep
}

// health es público (sin login) para monitores externos tipo UptimeRobot: responde 503 si algún cron está detenido.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	rep := s.healthStatus()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if !rep.OK {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	json.NewEncoder(w).Encode(rep)
}
