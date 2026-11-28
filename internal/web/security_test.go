package web

import (
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"world/internal/auth"
	"world/internal/store"
)

func newSecurityServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := &Server{st: st, pages: map[string]*template.Template{}, limiter: newLimiter(maxFailsPerIP, loginWindow),
		acctLim: newLimiter(maxFailsPerAcct, loginWindow), mfaLim: newLimiter(maxFailsPer2FA, mfaTTL),
		lockedLog: newLimiter(1, loginWindow), pwSem: make(chan struct{}, maxConcurrentPW), assetVer: "t"}
	if err := s.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	return s, st
}

func newUser(t *testing.T, st *store.Store, email, pw string) int64 {
	t.Helper()
	hash, err := auth.HashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.CreateUser(email, hash)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func post(h http.HandlerFunc, path, ip string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = hostPort(ip)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h(w, req)
	return w
}

func hostPort(ip string) string {
	if strings.Contains(ip, ":") {
		return "[" + ip + "]:4444"
	}
	return ip + ":4444"
}

func sessionCookieOf(w *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return c
		}
	}
	return nil
}

func loggedIn(w *httptest.ResponseRecorder) bool {
	return w.Code == http.StatusSeeOther && w.Header().Get("Location") == "/" && sessionCookieOf(w) != nil
}

func TestLoginLimits(t *testing.T) {
	s, st := newSecurityServer(t)
	newUser(t, st, "ana@miempresa.cl", "clave-correcta-1")
	login := func(ip, email, pw string) *httptest.ResponseRecorder {
		return post(s.loginSubmit, "/login", ip, url.Values{"email": {email}, "password": {pw}})
	}
	if w := login("203.0.113.10", "ana@miempresa.cl", "clave-correcta-1"); !loggedIn(w) {
		t.Fatalf("login inicial: %d %s", w.Code, w.Body.String())
	}
	// Un ataque repartido en muchas IPs: una falla por IP.
	for i := 1; i <= maxFailsPerAcct; i++ {
		if w := login(fmt.Sprintf("198.51.100.%d", i), "ANA@miempresa.cl", "mala"); !strings.Contains(w.Body.String(), "incorrectos") {
			t.Fatalf("intento %d: %s", i, w.Body.String())
		}
	}
	// La cuenta queda bloqueada para IPs desconocidas, aunque traigan la contraseña correcta...
	if w := login("198.51.100.200", "ana@miempresa.cl", "clave-correcta-1"); loggedIn(w) || !strings.Contains(w.Body.String(), "Demasiados") {
		t.Errorf("IP nueva debió quedar bloqueada: %d", w.Code)
	}
	// ...pero la dueña sigue entrando desde su IP conocida.
	if w := login("203.0.113.10", "ana@miempresa.cl", "clave-correcta-1"); !loggedIn(w) {
		t.Errorf("IP conocida bloqueada: %s", w.Body.String())
	}

	// Por IP: 10 fallas desde la misma red IPv6 /64, con correos distintos, bloquean toda la red.
	for i := 1; i <= maxFailsPerIP; i++ {
		login(fmt.Sprintf("2001:db8:1:2::%x", i), fmt.Sprintf("u%d@otraempresa.cl", i), "x")
	}
	if w := login("2001:db8:1:2::ffff", "otro@otraempresa.cl", "x"); !strings.Contains(w.Body.String(), "Demasiados") {
		t.Error("la red /64 debió quedar bloqueada")
	}
	if w := login("2001:db8:1:3::1", "otro@otraempresa.cl", "x"); strings.Contains(w.Body.String(), "Demasiados") {
		t.Error("otra red no debió bloquearse")
	}

	events, _ := st.LoginEvents(100)
	var locked, bad, ok, unknown int
	for _, e := range events {
		switch e.Result {
		case store.LoginLocked:
			locked++
		case store.LoginBadPassword:
			bad++
			if e.UserID == 0 {
				unknown++
			}
		case store.LoginOK:
			ok++
		}
	}
	if ok != 2 || bad != maxFailsPerAcct+maxFailsPerIP+1 || unknown != maxFailsPerIP+1 || locked != 2 {
		t.Errorf("historial: ok=%d mal=%d inexistentes=%d bloqueos=%d", ok, bad, unknown, locked)
	}
}

func TestLogin2FA(t *testing.T) {
	s, st := newSecurityServer(t)
	id := newUser(t, st, "ana@miempresa.cl", "clave-correcta-1")
	secret := auth.NewTOTPSecret()
	st.SetTOTP(id, secret, true)
	const ip = "203.0.113.10"
	start := func() *http.Cookie {
		w := post(s.loginSubmit, "/login", ip, url.Values{"email": {"ana@miempresa.cl"}, "password": {"clave-correcta-1"}})
		if w.Header().Get("Location") != "/login/2fa" {
			t.Fatalf("debió pedir 2FA: %d %s", w.Code, w.Header().Get("Location"))
		}
		return sessionCookieOf(w)
	}
	code, _ := auth.TOTPCodeAt(secret, time.Now())
	if w := post(s.mfaSubmit, "/login/2fa", ip, url.Values{"code": {code}}, start()); !loggedIn(w) {
		t.Fatalf("2FA correcto: %d %s", w.Code, w.Body.String())
	}
	// El mismo código no sirve dos veces.
	pending := start()
	if w := post(s.mfaSubmit, "/login/2fa", ip, url.Values{"code": {code}}, pending); loggedIn(w) || !strings.Contains(w.Body.String(), "una sola vez") {
		t.Errorf("código reutilizado aceptado: %d", w.Code)
	}
	// Tras 5 códigos malos con la misma contraseña, hay que volver a escribirla.
	var w *httptest.ResponseRecorder
	for i := 2; i <= maxFailsPer2FA; i++ {
		w = post(s.mfaSubmit, "/login/2fa", ip, url.Values{"code": {"000000"}}, pending)
	}
	if w.Header().Get("Location") != "/login" {
		t.Fatalf("debió volver al login: %d %s", w.Code, w.Header().Get("Location"))
	}
	if _, sess := s.sessionFromRequest(withCookie(pending)); sess != nil {
		t.Error("la sesión pendiente debió borrarse")
	}
	// Cada 2FA fallido queda como alerta: alguien tiene la contraseña.
	if alerts, _ := st.LoginAlerts(id, ""); len(alerts) != maxFailsPer2FA {
		t.Errorf("alertas: %d", len(alerts))
	}
}

func withCookie(c *http.Cookie) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(c)
	return r
}

func TestRequire2FA(t *testing.T) {
	s, st := newSecurityServer(t)
	newUser(t, st, "ana@miempresa.cl", "clave-correcta-1")
	w := post(s.loginSubmit, "/login", "203.0.113.10", url.Values{"email": {"ana@miempresa.cl"}, "password": {"clave-correcta-1"}})
	cookie := sessionCookieOf(w)
	reached := false
	h := s.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	get := func(path string) *httptest.ResponseRecorder {
		reached = false
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if get("/sites/1"); !reached {
		t.Fatal("sin 2FA obligatorio debe pasar")
	}
	st.SetSetting(settingRequire2FA, "1")
	if rec := get("/sites/1"); reached || rec.Header().Get("Location") != "/profile" {
		t.Errorf("debió mandar a configurar 2FA: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if get("/profile"); !reached {
		t.Error("el perfil debe quedar accesible para configurar 2FA")
	}
}

func TestLimiterBoundedAndIPKey(t *testing.T) {
	l := newLimiter(3, time.Minute)
	l.maxKeys = 100
	for i := 0; i < 1000; i++ {
		l.fail(fmt.Sprintf("k%d", i))
	}
	if len(l.hits) > 100 {
		t.Errorf("la tabla creció a %d claves", len(l.hits))
	}
	for i := 0; i < 10; i++ {
		l.fail("x")
	}
	if !l.blocked("x") || len(l.hits["x"]) != 3 {
		t.Errorf("x: %d", len(l.hits["x"]))
	}
	if ipKey("2001:db8:1:2:aaaa::1") != "2001:db8:1:2::/64" || ipKey("203.0.113.10") != "203.0.113.10" {
		t.Error("ipKey")
	}
	if !fromLoopback(&http.Request{RemoteAddr: "127.0.0.1:5000"}) || fromLoopback(&http.Request{RemoteAddr: "172.18.0.2:5000"}) {
		t.Error("fromLoopback")
	}
	if got := uaSummary("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/131.0 Safari/537.36 Edg/131.0"); got != "Edge en Windows" {
		t.Error(got)
	}
	if got := uaSummary("Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605 Version/18.0 Mobile Safari/604.1"); got != "Safari en iOS" {
		t.Error(got)
	}
}

func TestSecurityTemplates(t *testing.T) {
	s, st := newSecurityServer(t)
	id := newUser(t, st, "ana@miempresa.cl", "x")
	st.CreateSession("h1", id, "c", false, time.Now().Add(time.Hour), "203.0.113.10", "Mozilla/5.0 (Windows NT 10.0) Chrome/131")
	st.CreateSession("h2", id, "c", true, time.Now().Add(time.Hour), "198.51.100.20", "")
	st.AddLoginEvent(&store.LoginEvent{UserID: id, Email: "ana@miempresa.cl", IP: "203.0.113.10", IPKey: "203.0.113.10", Result: store.LoginOK, SessionHash: "h1"})
	st.AddLoginEvent(&store.LoginEvent{Email: "nadie@otraempresa.cl", IP: "198.51.100.20", Result: store.LoginBadPassword})
	st.AddLoginEvent(&store.LoginEvent{UserID: id, Email: "ana@miempresa.cl", IP: "198.51.100.77", Result: store.LoginBad2FA, SessionHash: "h2"})
	sessions, _ := st.UserSessions(id)
	events, _ := st.LoginEvents(10)
	alerts, _ := st.LoginAlerts(id, "h1")
	user, _ := st.UserByID(id)
	var out strings.Builder
	err := s.pages["security"].ExecuteTemplate(&out, "layout", data{"User": user, "CSRF": "t", "Path": "/security",
		"CurrentHash": "h1", "Sessions": sessions, "Events": events, "LoginAlerts": alerts, "Version": "v"})
	if err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{"esta sesión", "esperando código 2FA", "Chrome en Windows", `name="session" value="h2"`,
		"(no existe)", "contraseña correcta, 2FA falló", "Revisa estos accesos", "198.51.100.77", "Exigir 2FA a todos", "disabled"} {
		if !strings.Contains(html, want) {
			t.Errorf("falta %q", want)
		}
	}
	if strings.Contains(html, `value="h1"`) {
		t.Error("no debe ofrecer cerrar la sesión actual")
	}
	// En el login no se publica la versión.
	out.Reset()
	if err := s.pages["login"].ExecuteTemplate(&out, "layout", data{"AssetVer": "abc"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "world v") || !strings.Contains(out.String(), "app.css?v=abc") {
		t.Error("login con versión visible")
	}
}
