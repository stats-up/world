package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"world/internal/deploy"
	"world/internal/docker"
	"world/internal/store"
)

var (
	nameRe   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
	hostRe   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)
	branchRe = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._/-]{0,99}$`)
	sshRepo  = regexp.MustCompile(`^git@[A-Za-z0-9.-]+:[A-Za-z0-9._/-]+$`)
	extRe    = regexp.MustCompile(`^[a-z0-9_]+$`)

	phpVersions = []string{"8.4", "8.3", "8.2", "8.1"}
	siteKinds   = []struct{ Value, Label string }{
		{store.KindPHP, "PHP / Laravel"},
		{store.KindStatic, "Estático (HTML/JS)"},
		{store.KindDockerfile, "Dockerfile propio"},
	}
)

// --- Dashboard ---

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	sites, err := s.st.Sites()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d := data{"Sites": sites, "PanelDomain": s.st.Setting("panel_domain"), "Health": s.healthStatus()}
	if info, err := s.dc.Info(ctx); err == nil {
		d["Docker"] = info
		cs, _ := s.engine.Containers(ctx)
		if cs == nil {
			cs = map[string]docker.ContainerSummary{}
		}
		d["Containers"] = cs
		d["Traefik"] = s.proxy.Status(ctx)
	} else {
		d["DockerError"] = err.Error()
	}
	s.render(w, r, "dashboard", d)
}

// --- Formulario de sitio ---

func (s *Server) siteFormData(site *store.Site, env string, errs []string, isNew bool) data {
	return data{"Site": site, "Env": env, "Errors": errs, "New": isNew,
		"PHPVersions": phpVersions, "Kinds": siteKinds}
}

func (s *Server) siteNew(w http.ResponseWriter, r *http.Request) {
	site := &store.Site{Branch: "main", Kind: store.KindPHP, PHPVersion: "8.3", BuildAssets: true, Port: 8080}
	s.render(w, r, "site_form", s.siteFormData(site, "", nil, true))
}

func (s *Server) siteCreate(w http.ResponseWriter, r *http.Request) {
	site := &store.Site{Name: strings.ToLower(strings.TrimSpace(r.FormValue("name")))}
	env, errs := s.readSiteForm(r, site)
	if !nameRe.MatchString(site.Name) {
		errs = append([]string{"Nombre inválido: usa minúsculas, números y guiones (máx. 32)."}, errs...)
	}
	if len(errs) > 0 {
		s.render(w, r, "site_form", s.siteFormData(site, env, errs, true))
		return
	}
	priv, pub, err := deploy.GenerateDeployKey("world-" + site.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	site.DeployKeyPub = pub
	site.DeployKeyEnc = s.box.Seal(priv)
	site.EnvEnc = s.box.SealString(env)
	id, err := s.st.CreateSite(site)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			err = errors.New("ya existe un sitio con ese nombre")
		}
		s.render(w, r, "site_form", s.siteFormData(site, env, []string{err.Error()}, true))
		return
	}
	msg := "Sitio creado. Revisa la configuración y presiona Deploy."
	if site.UsesSSH() {
		msg = "Sitio creado. Agrega la deploy key en GitHub antes del primer deploy."
	}
	s.setFlash(w, "ok", msg)
	http.Redirect(w, r, fmt.Sprintf("/sites/%d", id), http.StatusSeeOther)
}

func (s *Server) siteEdit(w http.ResponseWriter, r *http.Request) {
	site, ok := s.loadSite(w, r)
	if !ok {
		return
	}
	env, err := s.box.OpenString(site.EnvEnc)
	if err != nil {
		http.Error(w, "no se pudieron descifrar las variables: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "site_form", s.siteFormData(site, env, nil, false))
}

func (s *Server) siteUpdate(w http.ResponseWriter, r *http.Request) {
	site, ok := s.loadSite(w, r)
	if !ok {
		return
	}
	env, errs := s.readSiteForm(r, site)
	if len(errs) > 0 {
		s.render(w, r, "site_form", s.siteFormData(site, env, errs, false))
		return
	}
	site.EnvEnc = s.box.SealString(env)
	if err := s.st.UpdateSite(site); err != nil {
		s.render(w, r, "site_form", s.siteFormData(site, env, []string{err.Error()}, false))
		return
	}
	s.setFlash(w, "ok", "Cambios guardados. Haz un deploy para aplicarlos.")
	http.Redirect(w, r, fmt.Sprintf("/sites/%d", site.ID), http.StatusSeeOther)
}

// readSiteForm carga los campos editables en site y devuelve el .env y los errores de validación.
func (s *Server) readSiteForm(r *http.Request, site *store.Site) (string, []string) {
	var errs []string
	site.RepoURL = strings.TrimSpace(r.FormValue("repo_url"))
	site.Branch = strings.TrimSpace(r.FormValue("branch"))
	site.Kind = r.FormValue("kind")
	site.PHPVersion = r.FormValue("php_version")
	site.BuildAssets = r.FormValue("build_assets") == "1"
	site.Autorun = r.FormValue("autorun") == "1"
	site.AutoDeploy = r.FormValue("auto_deploy") == "1"
	env := strings.ReplaceAll(r.FormValue("env"), "\r\n", "\n")

	if !validRepoURL(site.RepoURL) {
		errs = append(errs, "Repositorio inválido: usa https://github.com/usuario/repo.git (público) o git@github.com:usuario/repo.git (privado).")
	}
	if !branchRe.MatchString(site.Branch) {
		errs = append(errs, "Rama inválida.")
	}
	if !oneOf(site.Kind, store.KindPHP, store.KindStatic, store.KindDockerfile) {
		errs = append(errs, "Tipo de sitio inválido.")
	}
	if !oneOf(site.PHPVersion, phpVersions...) {
		site.PHPVersion = "8.3"
	}

	var exts []string
	for _, e := range strings.FieldsFunc(strings.ToLower(r.FormValue("php_extensions")), func(c rune) bool { return c == ',' || c == ' ' || c == '\n' }) {
		if !extRe.MatchString(e) {
			errs = append(errs, "Extensión PHP inválida: "+e)
			continue
		}
		exts = append(exts, e)
	}
	site.PHPExtensions = strings.Join(exts, " ")

	site.Port = 8080
	if site.Kind == store.KindDockerfile {
		p, err := strconv.Atoi(strings.TrimSpace(r.FormValue("port")))
		if err != nil || p < 1 || p > 65535 {
			errs = append(errs, "Puerto inválido.")
		} else {
			site.Port = p
		}
	}

	site.MemoryMB, _ = strconv.Atoi(strings.TrimSpace(r.FormValue("memory_mb")))
	if site.MemoryMB < 0 || (site.MemoryMB > 0 && site.MemoryMB < 64) {
		errs = append(errs, "El límite de memoria debe ser 0 (sin límite) o al menos 64 MB.")
	}
	site.CPUs, _ = strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(r.FormValue("cpus")), ",", "."), 64)
	if site.CPUs < 0 || site.CPUs > 64 {
		errs = append(errs, "Límite de CPU inválido.")
	}

	site.Domains = nil
	seen := map[string]bool{}
	for _, d := range store.SplitLines(strings.ToLower(r.FormValue("domains"))) {
		d = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(d, "https://"), "http://"), "/")
		if !hostRe.MatchString(d) {
			errs = append(errs, "Dominio inválido: "+d)
		}
		if !seen[d] { // los inválidos se conservan para re-mostrar el formulario; con errores no se guarda nada
			seen[d] = true
			site.Domains = append(site.Domains, d)
		}
	}
	if err := s.checkDomainConflicts(site); err != nil {
		errs = append(errs, err.Error())
	}
	if _, err := deploy.ParseEnv(env); err != nil {
		errs = append(errs, "Variables de ambiente: "+err.Error())
	}
	return env, errs
}

func validRepoURL(u string) bool {
	if sshRepo.MatchString(u) {
		return true
	}
	p, err := url.Parse(u)
	return err == nil && (p.Scheme == "https" || p.Scheme == "ssh") && p.Host != "" && !strings.HasPrefix(u, "-")
}

func (s *Server) checkDomainConflicts(site *store.Site) error {
	panel := s.st.Setting("panel_domain")
	others, err := s.st.Sites()
	if err != nil {
		return err
	}
	for _, d := range site.Domains {
		if d == panel {
			return fmt.Errorf("%s ya es el dominio del panel", d)
		}
		for _, o := range others {
			if o.ID == site.ID {
				continue
			}
			for _, od := range o.Domains {
				if od == d {
					return fmt.Errorf("%s ya está asignado al sitio %s", d, o.Name)
				}
			}
		}
	}
	return nil
}

func oneOf(v string, opts ...string) bool {
	for _, o := range opts {
		if v == o {
			return true
		}
	}
	return false
}

// --- Detalle y acciones ---

func (s *Server) loadSite(w http.ResponseWriter, r *http.Request) (*store.Site, bool) {
	id, err := pathID(r)
	if err != nil {
		http.NotFound(w, r)
		return nil, false
	}
	site, err := s.st.Site(id)
	if err != nil {
		http.NotFound(w, r)
		return nil, false
	}
	return site, true
}

func (s *Server) siteShow(w http.ResponseWriter, r *http.Request) {
	site, ok := s.loadSite(w, r)
	if !ok {
		return
	}
	deps, _ := s.st.Deployments(site.ID, 15)
	d := data{"Site": site, "Deployments": deps}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if cs, err := s.engine.Containers(ctx); err == nil {
		if c, ok := cs[site.Name]; ok {
			d["Container"] = c
		}
	} else {
		d["DockerError"] = err.Error()
	}
	s.render(w, r, "site", d)
}

func (s *Server) siteDeploy(w http.ResponseWriter, r *http.Request) {
	site, ok := s.loadSite(w, r)
	if !ok {
		return
	}
	id, err := s.engine.Start(site.ID, store.SourceManual)
	if err != nil {
		s.setFlash(w, "error", err.Error())
		redirect(w, r, fmt.Sprintf("/sites/%d", site.ID))
		return
	}
	redirect(w, r, fmt.Sprintf("/deployments/%d", id))
}

func (s *Server) siteControl(w http.ResponseWriter, r *http.Request) {
	site, ok := s.loadSite(w, r)
	if !ok {
		return
	}
	action := r.PathValue("action")
	labels := map[string]string{"start": "iniciado", "stop": "detenido", "restart": "reiniciado"}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := s.engine.Control(ctx, site, action); err != nil {
		s.setFlash(w, "error", err.Error())
	} else {
		s.setFlash(w, "ok", "Sitio "+labels[action]+".")
	}
	redirect(w, r, fmt.Sprintf("/sites/%d", site.ID))
}

func (s *Server) siteDelete(w http.ResponseWriter, r *http.Request) {
	site, ok := s.loadSite(w, r)
	if !ok {
		return
	}
	if r.FormValue("confirm") != site.Name {
		s.setFlash(w, "error", "Para eliminar, escribe el nombre exacto del sitio.")
		redirect(w, r, fmt.Sprintf("/sites/%d", site.ID))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if err := s.engine.Remove(ctx, site); err != nil {
		s.setFlash(w, "error", "No se pudo eliminar: "+err.Error())
		redirect(w, r, fmt.Sprintf("/sites/%d", site.ID))
		return
	}
	if deps, err := s.st.Deployments(site.ID, 100000); err == nil {
		for _, d := range deps {
			os.Remove(s.engine.LogPath(d.ID))
		}
	}
	if err := s.st.DeleteSite(site.ID); err != nil {
		s.setFlash(w, "error", err.Error())
	} else {
		s.setFlash(w, "ok", "Sitio "+site.Name+" eliminado.")
	}
	redirect(w, r, "/")
}

func (s *Server) siteLogs(w http.ResponseWriter, r *http.Request) {
	site, ok := s.loadSite(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	logs, err := s.engine.Logs(ctx, site, 300)
	if err != nil {
		logs = "Error: " + err.Error()
	}
	s.renderPartial(w, "container_logs", data{"Logs": logs, "SiteID": site.ID})
}

// --- Deploys ---

func (s *Server) loadDeployment(w http.ResponseWriter, r *http.Request) (*store.Deployment, *store.Site, bool) {
	id, err := pathID(r)
	if err != nil {
		http.NotFound(w, r)
		return nil, nil, false
	}
	dep, err := s.st.Deployment(id)
	if err != nil {
		http.NotFound(w, r)
		return nil, nil, false
	}
	site, err := s.st.Site(dep.SiteID)
	if err != nil {
		http.NotFound(w, r)
		return nil, nil, false
	}
	return dep, site, true
}

func (s *Server) deploymentShow(w http.ResponseWriter, r *http.Request) {
	dep, site, ok := s.loadDeployment(w, r)
	if !ok {
		return
	}
	s.render(w, r, "deployment", data{"Deployment": dep, "Site": site, "Log": s.readDeployLog(dep.ID)})
}

func (s *Server) deploymentLog(w http.ResponseWriter, r *http.Request) {
	dep, site, ok := s.loadDeployment(w, r)
	if !ok {
		return
	}
	s.renderPartial(w, "deploy_log", data{"Deployment": dep, "Site": site, "Log": s.readDeployLog(dep.ID)})
}

// readDeployLog devuelve como máximo los últimos 256 KB del log.
func (s *Server) readDeployLog(id int64) string {
	f, err := os.Open(s.engine.LogPath(id))
	if err != nil {
		return ""
	}
	defer f.Close()
	const max = 256 << 10
	if st, err := f.Stat(); err == nil && st.Size() > max {
		f.Seek(-max, io.SeekEnd)
	}
	b, _ := io.ReadAll(f)
	return string(b)
}

// --- Ajustes ---

func (s *Server) settingsForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "settings", s.settingsData(r, s.st.Setting("panel_domain"), s.st.Setting("acme_email"), ""))
}

func (s *Server) settingsData(r *http.Request, domain, email, errMsg string) data {
	d := data{"PanelDomain": domain, "ACMEEmail": email, "Error": errMsg, "Port": s.cfg.Port()}
	// Sugerencia para probar sin dominio propio: sslip.io resuelve "x.1-2-3-4.sslip.io" a 1.2.3.4.
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	if ip := net.ParseIP(host); ip != nil && ip.To4() != nil && !ip.IsLoopback() && !ip.IsPrivate() {
		d["SslipHint"] = "panel." + strings.ReplaceAll(ip.String(), ".", "-") + ".sslip.io"
	}
	return d
}

func (s *Server) settingsSubmit(w http.ResponseWriter, r *http.Request) {
	domain := strings.ToLower(strings.TrimSpace(r.FormValue("panel_domain")))
	email := strings.TrimSpace(r.FormValue("acme_email"))
	if domain != "" && !hostRe.MatchString(domain) {
		s.render(w, r, "settings", s.settingsData(r, domain, email, "Dominio inválido."))
		return
	}
	if email != "" {
		if _, err := mail.ParseAddress(email); err != nil {
			s.render(w, r, "settings", s.settingsData(r, domain, email, "Email inválido."))
			return
		}
	}
	if domain != "" {
		sites, _ := s.st.Sites()
		for _, site := range sites {
			for _, d := range site.Domains {
				if d == domain {
					s.render(w, r, "settings", s.settingsData(r, domain, email, "Ese dominio ya lo usa el sitio "+site.Name+"."))
					return
				}
			}
		}
	}
	s.st.SetSetting("panel_domain", domain)
	s.st.SetSetting("acme_email", email)

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	if err := s.proxy.Ensure(ctx, email, domain); err != nil {
		s.setFlash(w, "error", "Ajustes guardados, pero Traefik no se pudo actualizar: "+err.Error())
	} else if domain != "" {
		s.setFlash(w, "ok", "Guardado. En 1–2 minutos el panel estará en https://"+domain+" (el DNS debe apuntar a este servidor).")
	} else {
		s.setFlash(w, "ok", "Ajustes guardados.")
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}
