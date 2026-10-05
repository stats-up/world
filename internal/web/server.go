// Package web es la interfaz del panel: HTML renderizado en el servidor + htmx.
package web

import (
	"context"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"world/internal/auth"
	"world/internal/config"
	"world/internal/deploy"
	"world/internal/docker"
	"world/internal/proxy"
	"world/internal/secret"
	"world/internal/store"
)

//go:embed templates static
var assets embed.FS

const (
	sessionCookie = "world_session"
	flashCookie   = "world_flash"
	sessionTTL    = 7 * 24 * time.Hour
	mfaTTL        = 10 * time.Minute
)

type Server struct {
	cfg     config.Config
	st      *store.Store
	dc      *docker.Client
	box     *secret.Box
	engine  *deploy.Engine
	poller  *deploy.Poller
	proxy   *proxy.Manager
	version string

	pages    map[string]*template.Template
	partials *template.Template
	limiter  *limiter
}

func New(cfg config.Config, st *store.Store, dc *docker.Client, box *secret.Box, engine *deploy.Engine, poller *deploy.Poller, px *proxy.Manager, version string) (*Server, error) {
	s := &Server{cfg: cfg, st: st, dc: dc, box: box, engine: engine, poller: poller, proxy: px, version: version,
		pages: map[string]*template.Template{}, limiter: newLimiter(10, 15*time.Minute)}
	if err := s.parseTemplates(); err != nil {
		return nil, err
	}
	return s, nil
}

var funcs = template.FuncMap{
	"ago": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		d := time.Since(t)
		switch {
		case d < time.Minute:
			return "hace segundos"
		case d < time.Hour:
			return "hace " + strconv.Itoa(int(d.Minutes())) + " min"
		case d < 48*time.Hour:
			return "hace " + strconv.Itoa(int(d.Hours())) + " h"
		}
		return t.Format("2006-01-02 15:04")
	},
	"join": strings.Join,
	"gb":   func(b int64) string { return strconv.FormatFloat(float64(b)/(1<<30), 'f', 1, 64) + " GB" },
	"short": func(s string) string {
		if len(s) > 12 {
			return s[:12]
		}
		return s
	},
}

func (s *Server) parseTemplates() error {
	pages, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		return err
	}
	for _, p := range pages {
		name := strings.TrimSuffix(strings.TrimPrefix(p, "templates/"), ".html")
		if name == "layout" {
			continue
		}
		t, err := template.New("").Funcs(funcs).ParseFS(assets, "templates/layout.html", p, "templates/partials/*.html")
		if err != nil {
			return err
		}
		s.pages[name] = t
	}
	s.partials, err = template.New("").Funcs(funcs).ParseFS(assets, "templates/partials/*.html")
	return err
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(http.FileServerFS(static))))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok " + s.version)) })
	mux.HandleFunc("GET /health", s.health)

	mux.HandleFunc("GET /setup", s.setupForm)
	mux.HandleFunc("POST /setup", s.setupSubmit)
	mux.HandleFunc("GET /login", s.loginForm)
	mux.HandleFunc("POST /login", s.loginSubmit)
	mux.HandleFunc("GET /login/2fa", s.mfaForm)
	mux.HandleFunc("POST /login/2fa", s.mfaSubmit)

	priv := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.requireAuth(h)) }
	priv("POST /logout", s.logout)
	priv("GET /{$}", s.dashboard)
	priv("GET /sites/new", s.siteNew)
	priv("POST /sites", s.siteCreate)
	priv("GET /sites/{id}", s.siteShow)
	priv("GET /sites/{id}/edit", s.siteEdit)
	priv("POST /sites/{id}", s.siteUpdate)
	priv("POST /sites/{id}/deploy", s.siteDeploy)
	priv("POST /sites/{id}/control/{action}", s.siteControl)
	priv("POST /sites/{id}/delete", s.siteDelete)
	priv("GET /sites/{id}/logs", s.siteLogs)
	priv("GET /deployments/{id}", s.deploymentShow)
	priv("GET /deployments/{id}/log", s.deploymentLog)
	priv("GET /settings", s.settingsForm)
	priv("POST /settings", s.settingsSubmit)
	priv("GET /profile", s.profile)
	priv("POST /profile/password", s.profilePassword)
	priv("POST /profile/2fa/start", s.mfaStart)
	priv("POST /profile/2fa/enable", s.mfaEnable)
	priv("POST /profile/2fa/disable", s.mfaDisable)

	return securityHeaders(mux)
}

// --- Contexto de la request ---

type ctxKey int

const (
	userKey ctxKey = iota
	sessKey
	tokenKey
)

func currentUser(r *http.Request) *store.User {
	u, _ := r.Context().Value(userKey).(*store.User)
	return u
}

func currentSession(r *http.Request) *store.Session {
	sess, _ := r.Context().Value(sessKey).(*store.Session)
	return sess
}

func (s *Server) sessionFromRequest(r *http.Request) (string, *store.Session) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return "", nil
	}
	hash := auth.HashToken(c.Value)
	sess, err := s.st.Session(hash)
	if err != nil {
		return "", nil
	}
	return hash, sess
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hash, sess := s.sessionFromRequest(r)
		if sess == nil || sess.MFAPending {
			if n, _ := s.st.CountUsers(); n == 0 {
				http.Redirect(w, r, "/setup", http.StatusSeeOther)
				return
			}
			redirect(w, r, "/login")
			return
		}
		user, err := s.st.UserByID(sess.UserID)
		if err != nil {
			redirect(w, r, "/login")
			return
		}
		if r.Method != http.MethodGet {
			token := r.Header.Get("X-CSRF-Token")
			if token == "" {
				token = r.PostFormValue("csrf")
			}
			if !auth.SecureEqual(token, sess.CSRF) {
				http.Error(w, "Token CSRF inválido: recarga la página e intenta de nuevo.", http.StatusForbidden)
				return
			}
		}
		ctx := context.WithValue(r.Context(), userKey, user)
		ctx = context.WithValue(ctx, sessKey, sess)
		ctx = context.WithValue(ctx, tokenKey, hash)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// redirect funciona tanto para navegación normal como para requests de htmx.
func redirect(w http.ResponseWriter, r *http.Request, to string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", to)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// --- Renderizado ---

type data map[string]any

func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, d data) {
	t, ok := s.pages[page]
	if !ok {
		http.Error(w, "plantilla no encontrada: "+page, http.StatusInternalServerError)
		return
	}
	if d == nil {
		d = data{}
	}
	d["Version"] = s.version
	d["User"] = currentUser(r)
	if sess := currentSession(r); sess != nil {
		d["CSRF"] = sess.CSRF
	}
	if _, ok := d["Flash"]; !ok {
		d["Flash"] = s.popFlash(w, r)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout", d); err != nil {
		log.Printf("render %s: %v", page, err)
	}
}

func (s *Server) renderPartial(w http.ResponseWriter, name string, d any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.partials.ExecuteTemplate(w, name, d); err != nil {
		log.Printf("render partial %s: %v", name, err)
	}
}

type flash struct {
	Kind string // ok | error
	Msg  string
}

func (s *Server) setFlash(w http.ResponseWriter, kind, msg string) {
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: url.QueryEscape(kind + "|" + msg), Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 60})
}

func (s *Server) popFlash(w http.ResponseWriter, r *http.Request) *flash {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return nil
	}
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Path: "/", MaxAge: -1})
	v, _ := url.QueryUnescape(c.Value)
	kind, msg, ok := strings.Cut(v, "|")
	if !ok {
		return nil
	}
	return &flash{Kind: kind, Msg: msg}
}

// --- Utilidades HTTP ---

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	// Detrás de Traefik la conexión viene de una IP privada: usar la IP real que reenvía.
	if ip := net.ParseIP(host); ip != nil && (ip.IsPrivate() || ip.IsLoopback()) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	return host
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; form-action 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		next.ServeHTTP(w, r)
	})
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("id inválido")
	}
	return id, nil
}

// limiter limita intentos de login fallidos por IP.
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string][]time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{max: max, window: window, hits: map[string][]time.Time{}}
}

func (l *limiter) blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-l.window)
	var recent []time.Time
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	if len(recent) == 0 {
		delete(l.hits, key)
	} else {
		l.hits[key] = recent
	}
	return len(recent) >= l.max
}

func (l *limiter) fail(key string) {
	l.mu.Lock()
	l.hits[key] = append(l.hits[key], time.Now())
	l.mu.Unlock()
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	delete(l.hits, key)
	l.mu.Unlock()
}
