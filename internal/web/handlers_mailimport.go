package web

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"unicode"

	"world/internal/mail"
	"world/internal/store"
)

// Importar correos de otro servidor (imapsync) hacia un buzón. La contraseña de origen
// se pide cada vez y no se guarda.

// ResumeImports retoma las importaciones que seguían corriendo al reiniciar el panel.
func (s *Server) ResumeImports(ctx context.Context) { s.imports.Resume(ctx) }

func importBase(domainID, accountID string) string {
	return "/mail/domains/" + domainID + "/accounts/" + accountID
}

// addImports agrega a la página del buzón la importación en curso, el historial y el formulario.
func (s *Server) addImports(r *http.Request, dd data, d *mail.Domain, a *mail.Account) {
	imports, _ := s.st.MailImports(a.ID, 5)
	form := map[string]string{"host": "mail." + d.Name, "port": "993", "user": a.Email}
	if len(imports) > 0 {
		last := imports[0]
		form["host"], form["port"], form["user"] = last.Host, strconv.Itoa(last.Port), last.SrcUser
		if last.Running() {
			dd["Import"] = s.importData(d.ID, a.ID, last, sessionCSRF(r))
		} else {
			dd["LastImport"] = s.importData(d.ID, a.ID, last, sessionCSRF(r))
		}
	}
	dd["Imports"], dd["ImportForm"] = imports, form
}

func sessionCSRF(r *http.Request) string {
	if sess := currentSession(r); sess != nil {
		return sess.CSRF
	}
	return ""
}

func (s *Server) importData(domainID, accountID string, m *store.MailImport, csrf string) data {
	return data{"Import": m, "Log": lastLines(s.imports.Log(m.ID), 80), "Base": importBase(domainID, accountID), "CSRF": csrf}
}

// lastLines deja las últimas n líneas (el log de imapsync tiene una línea por correo).
func lastLines(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '\n' {
			if n--; n == 0 {
				return s[i+1:]
			}
		}
	}
	return s
}

func validImportHost(h string) bool {
	return hostRe.MatchString(h) || net.ParseIP(h) != nil
}

func validImportUser(u string) bool {
	if u == "" || len(u) > 256 {
		return false
	}
	for _, r := range u {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (s *Server) mailImportStart(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.mailCtx(r)
	defer cancel()
	d, a, ok := s.loadAccount(w, r, ctx)
	if !ok {
		return
	}
	back := importBase(d.ID, a.ID) + "#importar"
	host := strings.ToLower(strings.TrimSpace(r.FormValue("host")))
	user := strings.TrimSpace(r.FormValue("user"))
	password := r.FormValue("password")
	port, _ := strconv.Atoi(r.FormValue("port"))
	switch {
	case !validImportHost(host):
		s.mailBack(w, r, back, "error", "Servidor de origen inválido.")
		return
	case host == s.serverHostname() || host == mail.ContainerName:
		s.mailBack(w, r, back, "error", "El origen no puede ser este mismo servidor de correo.")
		return
	case port != 993 && port != 143:
		s.mailBack(w, r, back, "error", "Puerto inválido: usa 993 (SSL) o 143 (STARTTLS).")
		return
	case !validImportUser(user):
		s.mailBack(w, r, back, "error", "Usuario de origen inválido.")
		return
	case password == "" || len(password) > 1024 || strings.ContainsRune(password, 0):
		s.mailBack(w, r, back, "error", "Falta la contraseña del buzón en el servidor de origen.")
		return
	}
	_, err := s.imports.Start(ctx, mail.ImportRequest{AccountID: a.ID, Email: a.Email, Host: host, Port: port, User: user, Password: password})
	switch {
	case errors.Is(err, mail.ErrNotRunning):
		s.mailBack(w, r, back, "error", "El servidor de correo no está corriendo.")
	case err != nil:
		s.mailBack(w, r, back, "error", "No se pudo iniciar la importación: "+err.Error())
	default:
		s.mailBack(w, r, back, "ok", "Importación iniciada. Puedes cerrar esta página: sigue en segundo plano.")
	}
}

// loadImport valida la URL contra la importación (sin consultar Stalwart: se llama cada pocos segundos).
func (s *Server) loadImport(w http.ResponseWriter, r *http.Request) (*store.MailImport, bool) {
	id, aid := r.PathValue("id"), r.PathValue("aid")
	iid, err := strconv.ParseInt(r.PathValue("iid"), 10, 64)
	if !stalwartIDRe.MatchString(id) || !stalwartIDRe.MatchString(aid) || err != nil {
		http.NotFound(w, r)
		return nil, false
	}
	m, err := s.st.MailImport(iid)
	if err != nil || m.AccountID != aid {
		http.NotFound(w, r)
		return nil, false
	}
	return m, true
}

func (s *Server) mailImportStatus(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadImport(w, r)
	if !ok {
		return
	}
	if !m.Running() && r.Header.Get("HX-Request") == "true" {
		// Terminó: se recarga la página para mostrar el resultado y el formulario de nuevo.
		w.Header().Set("HX-Refresh", "true")
	}
	s.renderPartial(w, "mail_import", s.importData(r.PathValue("id"), r.PathValue("aid"), m, sessionCSRF(r)))
}

func (s *Server) mailImportCancel(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadImport(w, r)
	if !ok {
		return
	}
	back := importBase(r.PathValue("id"), r.PathValue("aid")) + "#importar"
	if !m.Running() {
		redirect(w, r, back)
		return
	}
	if err := s.imports.Cancel(r.Context(), m.ID); err != nil {
		s.mailBack(w, r, back, "error", "No se pudo cancelar: "+err.Error())
		return
	}
	s.mailBack(w, r, back, "ok", "Importación cancelada. Los correos ya copiados quedan en el buzón.")
}
