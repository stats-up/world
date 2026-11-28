package web

import (
	"encoding/base64"
	"html/template"
	"log"
	"net/http"
	"net/mail"
	"os"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"

	"world/internal/auth"
	"world/internal/store"
)

// --- Configuración inicial: crear el primer administrador ---

func (s *Server) setupNeeded() bool {
	n, err := s.st.CountUsers()
	return err == nil && n == 0
}

func (s *Server) setupForm(w http.ResponseWriter, r *http.Request) {
	if !s.setupNeeded() {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.render(w, r, "setup", data{"TokenPath": s.cfg.Path("setup-token")})
}

func (s *Server) setupSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.setupNeeded() {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	ip := ipKey(clientIP(r))
	fail := func(msg string) {
		s.render(w, r, "setup", data{"Error": msg, "Email": r.FormValue("email"), "TokenPath": s.cfg.Path("setup-token")})
	}
	if s.limiter.blocked(ip) {
		fail("Demasiados intentos. Espera 15 minutos.")
		return
	}
	want, err := os.ReadFile(s.cfg.Path("setup-token"))
	if err != nil || !auth.SecureEqual(strings.TrimSpace(string(want)), strings.TrimSpace(r.FormValue("token"))) {
		s.limiter.fail(ip)
		fail("El código de instalación no es correcto.")
		return
	}
	email := strings.TrimSpace(r.FormValue("email"))
	if _, err := mail.ParseAddress(email); err != nil {
		fail("Email inválido.")
		return
	}
	pw := r.FormValue("password")
	if msg := validatePassword(pw, r.FormValue("password_confirm")); msg != "" {
		fail(msg)
		return
	}
	hash, err := auth.HashPassword(pw)
	if err != nil {
		fail(err.Error())
		return
	}
	id, err := s.st.CreateUser(email, hash)
	if err != nil {
		fail(err.Error())
		return
	}
	os.Remove(s.cfg.Path("setup-token"))
	log.Printf("setup: administrador %s creado", email)
	sessHash := s.startSession(w, r, id, false)
	s.recordLogin(r, id, email, store.LoginOK, sessHash)
	s.setFlash(w, "ok", "Listo, bienvenido. Te recomendamos activar 2FA en tu perfil.")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func validatePassword(pw, confirm string) string {
	if len(pw) < auth.MinPasswordLen {
		return "La contraseña debe tener al menos 10 caracteres."
	}
	if pw != confirm {
		return "Las contraseñas no coinciden."
	}
	return ""
}

// --- Login ---

func (s *Server) loginForm(w http.ResponseWriter, r *http.Request) {
	if s.setupNeeded() {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if _, sess := s.sessionFromRequest(r); sess != nil && !sess.MFAPending {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, "login", nil)
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	ip := ipKey(clientIP(r))
	email := strings.TrimSpace(r.FormValue("email"))
	acct := acctKey(email)
	fail := func(msg string) { s.render(w, r, "login", data{"Error": msg, "Email": email}) }
	user, err := s.st.UserByEmail(email)
	if err != nil {
		user = nil
	}
	var uid int64
	if user != nil {
		uid = user.ID
	}
	// El límite por cuenta frena a quien prueba desde muchas IPs; no se aplica desde una IP con la
	// que la cuenta ya entró bien, para que un ataque no deje afuera al dueño.
	if s.limiter.blocked(ip) || (s.acctLim.blocked(acct) && (user == nil || !s.st.KnownLoginIP(user.ID, ip))) {
		s.recordLocked(r, acct, uid, email)
		fail("Demasiados intentos fallidos. Espera 15 minutos.")
		return
	}
	ok, err := s.checkPassword(r.Context(), user, r.FormValue("password"))
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		fail("El panel está recibiendo muchos intentos de acceso. Espera unos segundos e intenta de nuevo.")
		return
	}
	if !ok {
		s.limiter.fail(ip)
		s.acctLim.fail(acct)
		s.recordLogin(r, uid, email, store.LoginBadPassword, "")
		fail("Email o contraseña incorrectos.")
		return
	}
	if user.TOTPEnabled {
		s.startSession(w, r, user.ID, true)
		http.Redirect(w, r, "/login/2fa", http.StatusSeeOther)
		return
	}
	s.limiter.reset(ip)
	s.acctLim.reset(acct)
	hash := s.startSession(w, r, user.ID, false)
	s.recordLogin(r, user.ID, user.Email, store.LoginOK, hash)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) mfaForm(w http.ResponseWriter, r *http.Request) {
	if _, sess := s.sessionFromRequest(r); sess == nil || !sess.MFAPending {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.render(w, r, "login_2fa", nil)
}

func (s *Server) mfaSubmit(w http.ResponseWriter, r *http.Request) {
	hash, sess := s.sessionFromRequest(r)
	if sess == nil || !sess.MFAPending {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	ip := ipKey(clientIP(r))
	user, err := s.st.UserByID(sess.UserID)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	acct := acctKey(user.Email)
	if s.limiter.blocked(ip) || s.acctLim.blocked(acct) && !s.st.KnownLoginIP(user.ID, ip) {
		s.recordLocked(r, acct, user.ID, user.Email)
		s.render(w, r, "login_2fa", data{"Error": "Demasiados intentos fallidos. Espera 15 minutos."})
		return
	}
	if !s.checkCode(user, r.FormValue("code")) {
		s.limiter.fail(ip)
		s.acctLim.fail(acct)
		s.mfaLim.fail(hash)
		s.recordLogin(r, user.ID, user.Email, store.LoginBad2FA, hash)
		if s.mfaLim.blocked(hash) {
			// Demasiados códigos con la misma contraseña: hay que volver a escribirla.
			s.mfaLim.reset(hash)
			s.st.DeleteSession(hash)
			http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
			s.setFlash(w, "error", "Demasiados códigos incorrectos: vuelve a ingresar tu contraseña.")
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		s.render(w, r, "login_2fa", data{"Error": "Código incorrecto (cada código sirve una sola vez)."})
		return
	}
	s.limiter.reset(ip)
	s.acctLim.reset(acct)
	s.mfaLim.reset(hash)
	// Se emite una sesión nueva en lugar de promover la pendiente.
	s.st.DeleteSession(hash)
	newHash := s.startSession(w, r, user.ID, false)
	s.recordLogin(r, user.ID, user.Email, store.LoginOK, newHash)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// startSession crea la sesión, pone la cookie y devuelve el hash del token.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, userID int64, mfaPending bool) string {
	token := auth.RandomToken(32)
	ttl := sessionTTL
	if mfaPending {
		ttl = mfaTTL
	}
	hash := auth.HashToken(token)
	if err := s.st.CreateSession(hash, userID, auth.RandomToken(24), mfaPending, time.Now().Add(ttl),
		clientIP(r), clip(r.UserAgent(), 300)); err != nil {
		log.Printf("creando sesión: %v", err)
		return ""
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: isHTTPS(r),
		SameSite: http.SameSiteLaxMode, Expires: time.Now().Add(ttl),
	})
	return hash
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if hash, _ := r.Context().Value(tokenKey).(string); hash != "" {
		s.st.DeleteSession(hash)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
	redirect(w, r, "/login")
}

// --- Perfil: contraseña y 2FA ---

func (s *Server) profile(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "profile", nil)
}

func (s *Server) profilePassword(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if !auth.CheckPassword(user.PasswordHash, r.FormValue("current")) {
		s.render(w, r, "profile", data{"PwError": "La contraseña actual no es correcta."})
		return
	}
	pw := r.FormValue("password")
	if msg := validatePassword(pw, r.FormValue("password_confirm")); msg != "" {
		s.render(w, r, "profile", data{"PwError": msg})
		return
	}
	hash, err := auth.HashPassword(pw)
	if err == nil {
		err = s.st.SetPassword(user.ID, hash)
	}
	if err != nil {
		s.render(w, r, "profile", data{"PwError": err.Error()})
		return
	}
	token, _ := r.Context().Value(tokenKey).(string)
	s.st.DeleteUserSessions(user.ID, token)
	s.setFlash(w, "ok", "Contraseña actualizada. Se cerraron tus otras sesiones.")
	http.Redirect(w, r, "/profile", http.StatusSeeOther)
}

func (s *Server) mfaStart(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if user.TOTPEnabled {
		http.Redirect(w, r, "/profile", http.StatusSeeOther)
		return
	}
	secret := auth.NewTOTPSecret()
	if err := s.st.SetTOTP(user.ID, secret, false); err != nil {
		s.render(w, r, "profile", data{"MFAError": err.Error()})
		return
	}
	s.renderMFASetup(w, r, user.Email, secret, "")
}

func (s *Server) renderMFASetup(w http.ResponseWriter, r *http.Request, email, secret, errMsg string) {
	uri := auth.TOTPURL("world", email, secret)
	png, err := qrcode.Encode(uri, qrcode.Medium, 220)
	var qr template.URL
	if err == nil {
		qr = template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(png))
	}
	s.render(w, r, "profile", data{"MFASetup": true, "MFASecret": secret, "MFAQR": qr, "MFAError": errMsg})
}

func (s *Server) mfaEnable(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if user.TOTPSecret == "" || user.TOTPEnabled {
		http.Redirect(w, r, "/profile", http.StatusSeeOther)
		return
	}
	if !s.checkCode(user, r.FormValue("code")) {
		s.renderMFASetup(w, r, user.Email, user.TOTPSecret, "Código incorrecto. Revisa la hora del teléfono e intenta de nuevo.")
		return
	}
	if err := s.st.SetTOTP(user.ID, user.TOTPSecret, true); err != nil {
		s.render(w, r, "profile", data{"MFAError": err.Error()})
		return
	}
	s.setFlash(w, "ok", "2FA activado.")
	http.Redirect(w, r, "/profile", http.StatusSeeOther)
}

func (s *Server) mfaDisable(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if s.require2FA() {
		s.render(w, r, "profile", data{"MFAError": "El 2FA es obligatorio en este panel (se cambia en Seguridad)."})
		return
	}
	if !auth.CheckPassword(user.PasswordHash, r.FormValue("password")) || !s.checkCode(user, r.FormValue("code")) {
		s.render(w, r, "profile", data{"MFAError": "Contraseña o código incorrectos."})
		return
	}
	if err := s.st.SetTOTP(user.ID, "", false); err != nil {
		s.render(w, r, "profile", data{"MFAError": err.Error()})
		return
	}
	s.setFlash(w, "ok", "2FA desactivado.")
	http.Redirect(w, r, "/profile", http.StatusSeeOther)
}
