package web

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"world/internal/auth"
)

// Una sesión de consola se cierra sola pasado este tiempo, aunque siga en uso.
const consoleMaxDuration = 4 * time.Hour

// shellCmd abre bash si la imagen lo trae y, si no, sh.
var shellCmd = []string{"sh", "-c", "if command -v bash >/dev/null 2>&1; then exec bash -l; else exec sh -l; fi"}

func (s *Server) siteConsole(w http.ResponseWriter, r *http.Request) {
	site, ok := s.loadSite(w, r)
	if !ok {
		return
	}
	// xterm.js inserta sus propios <style> al dibujar el terminal: solo en esta página se permiten.
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'")
	s.render(w, r, "console", data{"Site": site, "Root": r.URL.Query().Get("user") == "root"})
}

// resizeMsg es el único mensaje de texto que envía el navegador; el teclado viaja como binario.
type resizeMsg struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

func (s *Server) siteConsoleWS(w http.ResponseWriter, r *http.Request) {
	site, ok := s.loadSite(w, r)
	if !ok {
		return
	}
	// El handshake es un GET: además de la cookie y del Origin (lo valida websocket.Accept), se exige el token CSRF.
	if !auth.SecureEqual(r.URL.Query().Get("csrf"), currentSession(r).CSRF) {
		http.Error(w, "Token CSRF inválido", http.StatusForbidden)
		return
	}
	user := ""
	if r.URL.Query().Get("user") == "root" {
		user = "root"
	}

	ctx, cancel := context.WithTimeout(context.Background(), consoleMaxDuration)
	defer cancel()

	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept ya respondió con el error
	}
	defer ws.CloseNow()
	ws.SetReadLimit(64 << 10)

	containerID, err := s.engine.RunningContainer(ctx, site)
	if err != nil {
		ws.Close(websocket.StatusNormalClosure, "El sitio no tiene un contenedor corriendo")
		return
	}
	cols, rows := 120, 32
	exec, err := s.dc.ExecAttach(ctx, containerID, shellCmd, user,
		[]string{"TERM=xterm-256color", "COLORTERM=truecolor"})
	if err != nil {
		ws.Close(websocket.StatusInternalError, truncate("No se pudo abrir la shell: "+err.Error(), 120))
		return
	}
	defer exec.Conn.Close()
	s.dc.ExecResize(ctx, exec.ID, cols, rows)

	who := currentUser(r).Email
	as := "usuario de la imagen"
	if user != "" {
		as = user
	}
	log.Printf("consola: %s abrió una shell en %s como %s (contenedor %.12s, IP %s)", who, site.Name, as, containerID, clientIP(r))
	start := time.Now()
	defer func() {
		log.Printf("consola: %s cerró la shell de %s (%s)", who, site.Name, time.Since(start).Round(time.Second))
	}()

	// Contenedor → navegador.
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 32<<10)
		for {
			n, err := exec.Conn.Read(buf)
			if n > 0 {
				if werr := ws.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Navegador → contenedor.
	go func() {
		defer cancel()
		for {
			typ, msg, err := ws.Read(ctx)
			if err != nil {
				return
			}
			if typ == websocket.MessageText {
				var m resizeMsg
				if json.Unmarshal(msg, &m) == nil && m.Type == "resize" && m.Cols > 0 && m.Rows > 0 && m.Cols < 1000 && m.Rows < 500 {
					s.dc.ExecResize(ctx, exec.ID, m.Cols, m.Rows)
				}
				continue
			}
			if _, err := exec.Conn.Write(msg); err != nil {
				return
			}
		}
	}()

	select {
	case <-done:
		ws.Close(websocket.StatusNormalClosure, "La sesión terminó")
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			ws.Close(websocket.StatusNormalClosure, "Se alcanzó el tiempo máximo de la sesión")
		}
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
