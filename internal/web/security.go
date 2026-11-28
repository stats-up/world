package web

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // hora de Chile aunque el servidor no tenga zonas horarias instaladas

	"world/internal/auth"
	"world/internal/store"
)

// Seguridad del acceso al panel: límites de intentos, 2FA obligatorio, códigos de un solo uso,
// historial de accesos, alertas de IPs nuevas y sesiones abiertas.

const (
	settingRequire2FA = "require_2fa"

	loginWindow     = 15 * time.Minute
	maxFailsPerIP   = 10 // por IP (o red /64 en IPv6)
	maxFailsPerAcct = 10 // por correo, venga de la IP que venga (frena ataques desde muchas IPs)
	maxFailsPer2FA  = 5  // códigos incorrectos con la misma contraseña antes de pedirla de nuevo
	maxLimiterKeys  = 50000
	maxConcurrentPW = 2 // verificaciones bcrypt a la vez: un ataque masivo no copa la CPU del servidor
	pwWait          = 10 * time.Second
	touchEvery      = 5 * time.Minute
)

// chileLoc: fechas del panel en hora de Chile.
var chileLoc = func() *time.Location {
	if loc, err := time.LoadLocation("America/Santiago"); err == nil {
		return loc
	}
	return time.UTC
}()

// --- Límite de intentos ---

// limiter cuenta intentos fallidos por clave dentro de una ventana de tiempo.
type limiter struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	maxKeys int
	hits    map[string][]time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{max: max, window: window, maxKeys: maxLimiterKeys, hits: map[string][]time.Time{}}
}

func (l *limiter) recent(key string, now time.Time) []time.Time {
	cutoff := now.Add(-l.window)
	var out []time.Time
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			out = append(out, t)
		}
	}
	return out
}

func (l *limiter) blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	recent := l.recent(key, time.Now())
	if len(recent) == 0 {
		delete(l.hits, key)
	} else {
		l.hits[key] = recent
	}
	return len(recent) >= l.max
}

func (l *limiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if _, ok := l.hits[key]; !ok && len(l.hits) >= l.maxKeys {
		l.sweep(now)
	}
	recent := append(l.recent(key, now), now)
	if len(recent) > l.max {
		recent = recent[len(recent)-l.max:] // basta con recordar los últimos max intentos
	}
	l.hits[key] = recent
}

// sweep borra las claves vencidas; si siguen sobrando (muchísimas IPs distintas), descarta
// algunas al azar para que la memoria no crezca sin límite.
func (l *limiter) sweep(now time.Time) {
	for k := range l.hits {
		if len(l.recent(k, now)) == 0 {
			delete(l.hits, k)
		}
	}
	for k := range l.hits {
		if len(l.hits) < l.maxKeys*9/10 {
			break
		}
		delete(l.hits, k)
	}
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	delete(l.hits, key)
	l.mu.Unlock()
}

// ipKey agrupa las IPv6 por su red /64: quien controla una red IPv6 tiene millones de direcciones.
func ipKey(ip string) string {
	p := net.ParseIP(ip)
	if p == nil || p.To4() != nil {
		return ip
	}
	return p.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

func acctKey(email string) string { return "acct:" + strings.ToLower(strings.TrimSpace(email)) }

// --- Códigos 2FA de un solo uso ---

// totpGuard recuerda el último intervalo usado por cada cuenta: un código ya usado (por ejemplo,
// visto por encima del hombro o capturado por una página falsa) no sirve una segunda vez.
type totpGuard struct {
	mu   sync.Mutex
	last map[int64]int64
}

func (g *totpGuard) use(userID, step int64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.last == nil {
		g.last = map[int64]int64{}
	}
	if step <= g.last[userID] {
		return false
	}
	g.last[userID] = step
	return true
}

// checkCode verifica el código 2FA de la cuenta y lo marca como usado.
func (s *Server) checkCode(user *store.User, code string) bool {
	step, ok := auth.MatchTOTP(user.TOTPSecret, code, time.Now())
	return ok && s.totp.use(user.ID, step)
}

// --- Contraseñas ---

var errBusy = errors.New("ocupado")

// checkPassword compara la contraseña con a lo más maxConcurrentPW verificaciones a la vez.
// Si el usuario no existe compara igual contra un hash ficticio: tarda lo mismo y no revela qué
// correos tienen cuenta.
func (s *Server) checkPassword(ctx context.Context, user *store.User, pw string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, pwWait)
	defer cancel()
	select {
	case s.pwSem <- struct{}{}:
	case <-ctx.Done():
		return false, errBusy
	}
	defer func() { <-s.pwSem }()
	if user == nil {
		auth.DummyCheckPassword(pw)
		return false, nil
	}
	return auth.CheckPassword(user.PasswordHash, pw), nil
}

// --- Historial de accesos ---

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func (s *Server) recordLogin(r *http.Request, userID int64, email, result, sessionHash string) {
	ip := clientIP(r)
	e := &store.LoginEvent{UserID: userID, Email: clip(strings.TrimSpace(email), 200), IP: ip, IPKey: ipKey(ip),
		UserAgent: clip(r.UserAgent(), 300), Result: result, SessionHash: sessionHash}
	if err := s.st.AddLoginEvent(e); err != nil {
		log.Printf("guardando acceso: %v", err)
	}
}

// recordLocked anota un intento rechazado por exceso, una vez por ventana y clave (si no, un ataque
// llenaría el historial).
func (s *Server) recordLocked(r *http.Request, key string, userID int64, email string) {
	if s.lockedLog.blocked(key) {
		return
	}
	s.lockedLog.fail(key)
	s.recordLogin(r, userID, email, store.LoginLocked, "")
}

// RunSecurity limpia cada hora las sesiones vencidas y el historial antiguo.
func (s *Server) RunSecurity(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		s.st.PurgeExpiredSessions()
		s.st.PurgeLoginEvents()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// --- 2FA obligatorio ---

func (s *Server) require2FA() bool { return s.st.Setting(settingRequire2FA) == "1" }

// needs2FASetup: con 2FA obligatorio, quien no lo tiene solo puede configurarlo o salir.
func (s *Server) needs2FASetup(user *store.User, r *http.Request) bool {
	if user.TOTPEnabled || !s.require2FA() {
		return false
	}
	switch r.URL.Path {
	case "/profile", "/profile/2fa/start", "/profile/2fa/enable", "/logout":
		return false
	}
	return true
}

// --- Página de seguridad ---

func (s *Server) security(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	hash, _ := r.Context().Value(tokenKey).(string)
	sessions, err := s.st.UserSessions(user.ID)
	d := data{"Require2FA": s.require2FA(), "CurrentHash": hash, "Sessions": sessions}
	if err != nil {
		d["Error"] = err.Error()
	}
	events, err := s.st.LoginEvents(200)
	if err != nil {
		d["Error"] = err.Error()
	}
	d["Events"] = events
	s.render(w, r, "security", d)
}

func (s *Server) securityRequire2FA(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	on := r.FormValue("require") == "1"
	if on && !user.TOTPEnabled {
		s.setFlash(w, "error", "Activa primero tu propio 2FA en Perfil: si no, quedarías obligado a configurarlo antes de seguir.")
		redirect(w, r, "/security")
		return
	}
	if err := s.st.SetSetting(settingRequire2FA, map[bool]string{true: "1", false: "0"}[on]); err != nil {
		s.setFlash(w, "error", err.Error())
	} else if on {
		log.Printf("seguridad: %s exigió 2FA a todos los usuarios", user.Email)
		s.setFlash(w, "ok", "2FA obligatorio: quien no lo tenga deberá configurarlo al entrar.")
	} else {
		log.Printf("seguridad: %s dejó de exigir 2FA", user.Email)
		s.setFlash(w, "ok", "El 2FA ya no es obligatorio.")
	}
	redirect(w, r, "/security")
}

func (s *Server) securitySessionClose(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	current, _ := r.Context().Value(tokenKey).(string)
	target := r.FormValue("session")
	if target == current {
		s.setFlash(w, "error", "Esta es tu sesión actual: usa Salir.")
	} else if ok, err := s.st.DeleteUserSession(user.ID, target); err != nil {
		s.setFlash(w, "error", err.Error())
	} else if ok {
		s.setFlash(w, "ok", "Sesión cerrada.")
	}
	redirect(w, r, "/security")
}

func (s *Server) securitySessionsCloseOthers(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	current, _ := r.Context().Value(tokenKey).(string)
	if err := s.st.DeleteUserSessions(user.ID, current); err != nil {
		s.setFlash(w, "error", err.Error())
	} else {
		log.Printf("seguridad: %s cerró sus otras sesiones", user.Email)
		s.setFlash(w, "ok", "Se cerraron todas tus otras sesiones.")
	}
	redirect(w, r, "/security")
}

func (s *Server) securityAlertsAck(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	var err error
	if id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64); id > 0 {
		err = s.st.AckLoginAlert(user.ID, id)
	} else {
		err = s.st.AckAllLoginAlerts(user.ID)
	}
	if err != nil {
		s.setFlash(w, "error", err.Error())
	}
	back := r.FormValue("back")
	if !strings.HasPrefix(back, "/") || strings.HasPrefix(back, "//") {
		back = "/security"
	}
	redirect(w, r, back)
}

// uaSummary resume el navegador y el sistema: "Chrome en Windows".
func uaSummary(ua string) string {
	if ua == "" {
		return "desconocido"
	}
	browser := "Navegador"
	switch {
	case strings.Contains(ua, "Edg/"):
		browser = "Edge"
	case strings.Contains(ua, "OPR/"):
		browser = "Opera"
	case strings.Contains(ua, "Firefox/"):
		browser = "Firefox"
	case strings.Contains(ua, "Chrome/"), strings.Contains(ua, "CriOS/"):
		browser = "Chrome"
	case strings.Contains(ua, "Safari/"):
		browser = "Safari"
	case strings.HasPrefix(ua, "curl/"), strings.Contains(ua, "python"), strings.Contains(ua, "Go-http-client"):
		return "programa (" + clip(ua, 40) + ")"
	}
	osName := ""
	switch {
	case strings.Contains(ua, "Android"):
		osName = "Android"
	case strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPad"):
		osName = "iOS"
	case strings.Contains(ua, "Windows"):
		osName = "Windows"
	case strings.Contains(ua, "Mac OS X"), strings.Contains(ua, "Macintosh"):
		osName = "macOS"
	case strings.Contains(ua, "Linux"):
		osName = "Linux"
	}
	if osName == "" {
		return browser
	}
	return browser + " en " + osName
}
