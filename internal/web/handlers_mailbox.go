package web

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"world/internal/mail"
)

// Sección Correo: dominios y buzones de Stalwart. La fuente de verdad es Stalwart (vía su API);
// world no guarda copia de dominios ni buzones.

var (
	stalwartIDRe = regexp.MustCompile(`^[a-z0-9]{1,32}$`)
	localPartRe  = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,62}[a-z0-9])?$`)
)

const defaultQuotaGB = 5

func (s *Server) mailCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 30*time.Second)
}

// mailUnavailable muestra la página de correo con el motivo cuando Stalwart no responde.
func (s *Server) mailError(w http.ResponseWriter, r *http.Request, err error) {
	msg := err.Error()
	if errors.Is(err, mail.ErrNotRunning) {
		msg = "El servidor de correo no está corriendo. Actívalo en Ajustes → Correo."
	}
	s.render(w, r, "mail", data{"Error": msg})
}

func (s *Server) mailBack(w http.ResponseWriter, r *http.Request, to, kind, msg string) {
	s.setFlash(w, kind, msg)
	redirect(w, r, to)
}

func (s *Server) serverHostname() string { return mail.LoadSettings(s.st, s.box).Hostname }

// --- Dominios ---

func (s *Server) mailIndex(w http.ResponseWriter, r *http.Request) {
	if !mail.Enabled(s.st) {
		s.render(w, r, "mail", data{"Disabled": true})
		return
	}
	ctx, cancel := s.mailCtx(r)
	defer cancel()
	all, err := s.mailAPI.Domains(ctx)
	if err != nil {
		s.mailError(w, r, err)
		return
	}
	host := s.serverHostname()
	var domains []mail.Domain
	for _, d := range all {
		if !mail.IsServerDomain(d.Name, host) {
			domains = append(domains, d)
		}
	}
	s.render(w, r, "mail", data{"Domains": domains, "Hostname": host})
}

// mailDomainNew muestra el formulario de dominio nuevo (también al volver con un error).
func (s *Server) mailDomainNew(w http.ResponseWriter, r *http.Request) {
	if !mail.Enabled(s.st) {
		redirect(w, r, "/mail")
		return
	}
	s.render(w, r, "mail_domain_new", data{"Hostname": s.serverHostname(), "Form": map[string]string{"mode": "migration"}})
}

func (s *Server) mailDomainCreate(w http.ResponseWriter, r *http.Request) {
	name := strings.ToLower(strings.TrimSpace(r.FormValue("name")))
	origin := strings.ToLower(strings.TrimSpace(r.FormValue("origin")))
	desc := strings.TrimSpace(r.FormValue("description"))
	active := r.FormValue("mode") == "active"
	fail := func(msg string) {
		mode := "migration"
		if active {
			mode = "active"
		}
		s.render(w, r, "mail_domain_new", data{"Hostname": s.serverHostname(), "Error": msg,
			"Form": map[string]string{"name": name, "origin": r.FormValue("origin"), "description": desc, "mode": mode}})
	}
	if !hostRe.MatchString(name) {
		fail("Dominio inválido.")
		return
	}
	if origin == "" {
		origin = mail.DefaultOrigin(name)
	}
	if origin != "" && (!hostRe.MatchString(origin) || (origin != name && !strings.HasSuffix(name, "."+origin))) {
		fail("La zona DNS debe ser el dominio o uno de sus dominios padre (ej: frikiforja.cl).")
		return
	}
	if origin == name {
		origin = ""
	}
	if name == s.serverHostname() {
		fail("Ese es el nombre del servidor de correo, no un dominio de correo.")
		return
	}
	ctx, cancel := s.mailCtx(r)
	defer cancel()
	id, err := s.mailAPI.CreateDomain(ctx, name, origin, desc, active)
	if err != nil {
		fail("No se pudo crear el dominio: " + err.Error())
		return
	}
	s.mailBack(w, r, "/mail/domains/"+id, "ok", "Dominio "+name+" creado. Stalwart está publicando sus registros y pidiendo el certificado (1–2 minutos).")
}

// loadDomain valida el id de la URL y trae el dominio (escribe la respuesta si falla).
func (s *Server) loadDomain(w http.ResponseWriter, r *http.Request, ctx context.Context) (*mail.Domain, bool) {
	id := r.PathValue("id")
	if !stalwartIDRe.MatchString(id) {
		http.NotFound(w, r)
		return nil, false
	}
	d, err := s.mailAPI.Domain(ctx, id)
	if errors.Is(err, mail.ErrNotFound) || (err == nil && mail.IsServerDomain(d.Name, s.serverHostname())) {
		http.NotFound(w, r)
		return nil, false
	}
	if err != nil {
		s.mailError(w, r, err)
		return nil, false
	}
	return d, true
}

func (s *Server) mailDomainShow(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.mailCtx(r)
	defer cancel()
	d, ok := s.loadDomain(w, r, ctx)
	if !ok {
		return
	}
	accounts, err := s.mailAPI.Accounts(ctx, d.ID)
	if err != nil {
		s.mailError(w, r, err)
		return
	}
	tab := r.URL.Query().Get("tab")
	if tab != "dns" && tab != "ajustes" {
		tab = "buzones"
	}
	dd := data{"Domain": d, "Accounts": accounts, "Tab": tab, "Hostname": s.serverHostname()}
	if tab == "dns" { // la verificación consulta el DNS público: solo en su pestaña
		records := mail.CheckRecords(ctx, mail.Resolver, mail.ParseZone(d.ZoneFile))
		pending := 0
		for _, rec := range records {
			if rec.Status != "ok" {
				pending++
			}
		}
		dd["Records"], dd["Pending"] = records, pending
	}
	s.render(w, r, "mail_domain", dd)
}

func (s *Server) mailDomainMode(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.mailCtx(r)
	defer cancel()
	d, ok := s.loadDomain(w, r, ctx)
	if !ok {
		return
	}
	active := r.FormValue("mode") == "active"
	back := "/mail/domains/" + d.ID + "?tab=ajustes"
	if err := s.mailAPI.SetDomainActive(ctx, d.ID, active); err != nil {
		s.mailBack(w, r, back, "error", "No se pudo cambiar el modo: "+err.Error())
		return
	}
	if active {
		s.mailBack(w, r, back, "ok", "Dominio activo: Stalwart publicará el MX hacia este servidor en Cloudflare.")
	} else {
		s.mailBack(w, r, back, "ok", "Dominio en migración: Stalwart deja de administrar el MX. Revisa en Cloudflare a dónde apunta.")
	}
}

func (s *Server) mailDomainDelete(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.mailCtx(r)
	defer cancel()
	d, ok := s.loadDomain(w, r, ctx)
	if !ok {
		return
	}
	if err := s.mailAPI.DeleteDomain(ctx, d.ID); err != nil {
		s.mailBack(w, r, "/mail/domains/"+d.ID+"?tab=ajustes", "error", "No se pudo eliminar: "+err.Error())
		return
	}
	s.mailBack(w, r, "/mail", "ok", "Dominio "+d.Name+" eliminado.")
}

// --- Buzones ---

// parseAliases acepta partes locales separadas por comas, espacios o saltos de línea.
func parseAliases(in, own string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, a := range strings.FieldsFunc(strings.ToLower(in), func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == ';' }) {
		a = strings.TrimSpace(a)
		if i := strings.IndexByte(a, '@'); i >= 0 {
			a = a[:i] // los alias son del mismo dominio
		}
		if a == "" || a == own || seen[a] {
			continue
		}
		if !localPartRe.MatchString(a) {
			return nil, errors.New("alias inválido: " + a)
		}
		seen[a] = true
		out = append(out, a)
	}
	return out, nil
}

// parseQuotaGB: GB enteros o con decimales; 0 = sin límite.
func parseQuotaGB(in string) (int64, error) {
	in = strings.TrimSpace(strings.ReplaceAll(in, ",", "."))
	if in == "" {
		return 0, nil
	}
	gb, err := strconv.ParseFloat(in, 64)
	if err != nil || gb < 0 || gb > 10000 {
		return 0, errors.New("cuota inválida")
	}
	return int64(gb * (1 << 30)), nil
}

// generatePassword: 20 caracteres sin los que se confunden al copiarlos (0/O, 1/l/I).
func generatePassword() string {
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 20)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			panic(err)
		}
		b[i] = alphabet[n.Int64()]
	}
	return string(b)
}

// renderPassword muestra la contraseña una sola vez (no se guarda en world ni se puede recuperar).
func (s *Server) renderPassword(w http.ResponseWriter, r *http.Request, d *mail.Domain, email, password string, created bool) {
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, r, "mail_password", data{"Domain": d, "Email": email, "Password": password, "Created": created, "Hostname": s.serverHostname()})
}

// mailAccountNew muestra el formulario de buzón nuevo.
func (s *Server) mailAccountNew(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.mailCtx(r)
	defer cancel()
	d, ok := s.loadDomain(w, r, ctx)
	if !ok {
		return
	}
	s.render(w, r, "mail_account_new", data{"Domain": d, "Form": map[string]string{"quota_gb": strconv.Itoa(defaultQuotaGB)}})
}

func (s *Server) mailAccountCreate(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.mailCtx(r)
	defer cancel()
	d, ok := s.loadDomain(w, r, ctx)
	if !ok {
		return
	}
	name := strings.ToLower(strings.TrimSpace(r.FormValue("name")))
	if i := strings.IndexByte(name, '@'); i >= 0 {
		name = name[:i]
	}
	fail := func(msg string) {
		s.render(w, r, "mail_account_new", data{"Domain": d, "Error": msg, "Form": map[string]string{"name": name,
			"quota_gb": r.FormValue("quota_gb"), "description": r.FormValue("description"), "aliases": r.FormValue("aliases")}})
	}
	if !localPartRe.MatchString(name) {
		fail("Nombre de buzón inválido: usa minúsculas, números, punto, guion o guion bajo.")
		return
	}
	quota, err := parseQuotaGB(r.FormValue("quota_gb"))
	if err != nil {
		fail(err.Error())
		return
	}
	aliases, err := parseAliases(r.FormValue("aliases"), name)
	if err != nil {
		fail(err.Error())
		return
	}
	password := generatePassword()
	_, err = s.mailAPI.CreateAccount(ctx, mail.NewAccount{DomainID: d.ID, Name: name, Description: strings.TrimSpace(r.FormValue("description")),
		Password: password, Quota: quota, Aliases: aliases})
	if err != nil {
		fail("No se pudo crear el buzón: " + err.Error())
		return
	}
	s.renderPassword(w, r, d, name+"@"+d.Name, password, true)
}

func (s *Server) loadAccount(w http.ResponseWriter, r *http.Request, ctx context.Context) (*mail.Domain, *mail.Account, bool) {
	d, ok := s.loadDomain(w, r, ctx)
	if !ok {
		return nil, nil, false
	}
	aid := r.PathValue("aid")
	if !stalwartIDRe.MatchString(aid) {
		http.NotFound(w, r)
		return nil, nil, false
	}
	a, err := s.mailAPI.Account(ctx, aid)
	if errors.Is(err, mail.ErrNotFound) || (err == nil && !strings.HasSuffix(a.Email, "@"+d.Name)) {
		http.NotFound(w, r)
		return nil, nil, false
	}
	if err != nil {
		s.mailError(w, r, err)
		return nil, nil, false
	}
	return d, a, true
}

func (s *Server) mailAccountShow(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.mailCtx(r)
	defer cancel()
	d, a, ok := s.loadAccount(w, r, ctx)
	if !ok {
		return
	}
	quota := ""
	if a.Quota > 0 {
		quota = strconv.FormatFloat(float64(a.Quota)/(1<<30), 'f', -1, 64)
	}
	s.render(w, r, "mail_account", data{"Domain": d, "Account": a, "QuotaGB": quota, "Aliases": strings.Join(a.Aliases, ", "), "Hostname": s.serverHostname()})
}

func (s *Server) mailAccountUpdate(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.mailCtx(r)
	defer cancel()
	d, a, ok := s.loadAccount(w, r, ctx)
	if !ok {
		return
	}
	back := "/mail/domains/" + d.ID + "/accounts/" + a.ID
	quota, err := parseQuotaGB(r.FormValue("quota_gb"))
	if err != nil {
		s.mailBack(w, r, back, "error", err.Error())
		return
	}
	aliases, err := parseAliases(r.FormValue("aliases"), a.Name)
	if err != nil {
		s.mailBack(w, r, back, "error", err.Error())
		return
	}
	if err := s.mailAPI.UpdateAccount(ctx, a.ID, d.ID, strings.TrimSpace(r.FormValue("description")), quota, aliases); err != nil {
		s.mailBack(w, r, back, "error", "No se pudo guardar: "+err.Error())
		return
	}
	s.mailBack(w, r, back, "ok", "Buzón actualizado.")
}

func (s *Server) mailAccountPassword(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.mailCtx(r)
	defer cancel()
	d, a, ok := s.loadAccount(w, r, ctx)
	if !ok {
		return
	}
	password := generatePassword()
	if err := s.mailAPI.SetPassword(ctx, a.ID, password); err != nil {
		s.mailBack(w, r, "/mail/domains/"+d.ID+"/accounts/"+a.ID, "error", "No se pudo cambiar la contraseña: "+err.Error())
		return
	}
	s.renderPassword(w, r, d, a.Email, password, false)
}

func (s *Server) mailAccountDelete(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.mailCtx(r)
	defer cancel()
	d, a, ok := s.loadAccount(w, r, ctx)
	if !ok {
		return
	}
	if r.FormValue("confirm") != a.Email {
		s.mailBack(w, r, "/mail/domains/"+d.ID+"/accounts/"+a.ID, "error", "Para eliminar, escribe la dirección exacta del buzón.")
		return
	}
	if err := s.mailAPI.DeleteAccount(ctx, a.ID); err != nil {
		s.mailBack(w, r, "/mail/domains/"+d.ID+"/accounts/"+a.ID, "error", "No se pudo eliminar: "+err.Error())
		return
	}
	s.mailBack(w, r, "/mail/domains/"+d.ID, "ok", "Buzón "+a.Email+" eliminado con todos sus correos.")
}
