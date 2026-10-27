package store

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Tipos de sitio soportados.
const (
	KindPHP        = "php"        // Laravel / PHP genérico con imagen base del panel
	KindStatic     = "static"     // HTML/JS estático servido por nginx
	KindDockerfile = "dockerfile" // el repo trae su propio Dockerfile
)

type Site struct {
	ID            int64
	Name          string
	RepoURL       string
	Branch        string
	Kind          string
	PHPVersion    string
	PHPExtensions string // separadas por espacio
	BuildAssets   bool   // npm run build si hay package.json
	Autorun       bool   // migraciones + caches de Laravel al arrancar
	Port          int    // puerto interno (solo para kind=dockerfile)
	Domains       []string
	EnvEnc        []byte
	DeployKeyPub  string
	DeployKeyEnc  []byte
	MemoryMB      int
	CPUs          float64
	CurrentImage  string
	CreatedAt     time.Time

	// Auto-deploy: world consulta la rama cada minuto y despliega si hay un commit nuevo.
	AutoDeploy     bool
	LastCheckAt    time.Time
	LastRemoteSHA  string
	LastCheckError string

	// Carpetas del contenedor que se guardan en volúmenes de Docker y sobreviven a los deploys.
	PersistPaths []string
	// RedirectAliases: los dominios distintos del primero redirigen (301) al dominio principal.
	RedirectAliases bool
	// RedirectDomains: sin RedirectAliases, los alias que igual redirigen al principal (ej. solo el www).
	RedirectDomains []string

	// Cron: comando que world ejecuta cada minuto dentro del contenedor (ej: php artisan schedule:run).
	CronCommand    string
	LastCronAt     time.Time
	LastCronExit   int
	LastCronMS     int64
	LastCronOutput string

	// DeployMode: cómo se reemplaza el contenedor (DeployRolling o DeployRecreate).
	DeployMode string
	// StopTimeout: segundos que se espera a que el contenedor cierre al detenerlo (0 = por defecto).
	StopTimeout int
}

const (
	// DeployRolling: el contenedor nuevo arranca junto al anterior y lo reemplaza cuando está sano (sin corte).
	DeployRolling = "rolling"
	// DeployRecreate: nunca corren dos a la vez. Se construye la imagen, se espera a que el cron termine,
	// se detiene el anterior y recién entonces arranca el nuevo (corte breve). Para servicios que no
	// admiten dos instancias: colas, sesiones de WhatsApp, bots, workers.
	DeployRecreate = "recreate"

	DefaultStopTimeout = 15
	MaxStopTimeout     = 300
)

func (s *Site) StopSecs() int { return int(s.StopWait().Seconds()) }

func (s *Site) Recreate() bool { return s.DeployMode == DeployRecreate }

// StopWait es cuánto se espera a que el contenedor cierre antes de forzarlo.
func (s *Site) StopWait() time.Duration {
	if s.StopTimeout <= 0 {
		return DefaultStopTimeout * time.Second
	}
	return time.Duration(min(s.StopTimeout, MaxStopTimeout)) * time.Second
}

// PrimaryDomain es el primer dominio de la lista (el principal).
func (s *Site) PrimaryDomain() string {
	if len(s.Domains) == 0 {
		return ""
	}
	return s.Domains[0]
}

// Aliases son los dominios que no son el principal.
func (s *Site) Aliases() []string {
	if len(s.Domains) < 2 {
		return nil
	}
	return s.Domains[1:]
}

// Redirects indica si el dominio redirige (301) al principal en vez de llegar a la app.
func (s *Site) Redirects(domain string) bool {
	if len(s.Domains) < 2 || domain == s.Domains[0] || !slices.Contains(s.Domains, domain) {
		return false
	}
	return s.RedirectAliases || slices.Contains(s.RedirectDomains, domain)
}

// Redirected son los alias que redirigen al principal; Served, los dominios que llegan a la app.
func (s *Site) Redirected() []string { return s.splitDomains(true) }
func (s *Site) Served() []string     { return s.splitDomains(false) }

func (s *Site) splitDomains(redirected bool) []string {
	var out []string
	for _, d := range s.Domains {
		if s.Redirects(d) == redirected {
			out = append(out, d)
		}
	}
	return out
}

// UsesSSH indica si el repo se clona por SSH (privado, con deploy key).
func (s *Site) UsesSSH() bool {
	return strings.HasPrefix(s.RepoURL, "git@") || strings.HasPrefix(s.RepoURL, "ssh://")
}

// InternalPort es el puerto donde escucha la app dentro del contenedor.
func (s *Site) InternalPort() int {
	switch s.Kind {
	case KindPHP:
		return 8080
	case KindStatic:
		return 80
	default:
		return s.Port
	}
}

const siteCols = `id, name, repo_url, branch, kind, php_version, php_extensions, build_assets, autorun, port,
	domains, env_enc, deploy_key_pub, deploy_key_enc, memory_mb, cpus, current_image, created_at,
	auto_deploy, last_check_at, last_remote_sha, last_check_error,
	persist_paths, redirect_aliases, cron_command, last_cron_at, last_cron_exit, last_cron_ms, last_cron_output,
	deploy_mode, stop_timeout, redirect_domains`

func scanSite(row interface{ Scan(...any) error }) (*Site, error) {
	var s Site
	var assets, autorun, auto, redirect int
	var domains, persist, redirectDomains string
	var created, checked, cronAt int64
	err := row.Scan(&s.ID, &s.Name, &s.RepoURL, &s.Branch, &s.Kind, &s.PHPVersion, &s.PHPExtensions,
		&assets, &autorun, &s.Port, &domains, &s.EnvEnc, &s.DeployKeyPub, &s.DeployKeyEnc,
		&s.MemoryMB, &s.CPUs, &s.CurrentImage, &created,
		&auto, &checked, &s.LastRemoteSHA, &s.LastCheckError,
		&persist, &redirect, &s.CronCommand, &cronAt, &s.LastCronExit, &s.LastCronMS, &s.LastCronOutput,
		&s.DeployMode, &s.StopTimeout, &redirectDomains)
	if err != nil {
		return nil, notFound(err)
	}
	s.BuildAssets = assets == 1
	s.Autorun = autorun == 1
	s.AutoDeploy = auto == 1
	s.RedirectAliases = redirect == 1
	s.Domains = SplitLines(domains)
	s.RedirectDomains = SplitLines(redirectDomains)
	s.PersistPaths = SplitLines(persist)
	if cronAt > 0 {
		s.LastCronAt = time.Unix(cronAt, 0)
	}
	s.CreatedAt = time.Unix(created, 0)
	if checked > 0 {
		s.LastCheckAt = time.Unix(checked, 0)
	}
	return &s, nil
}

func (s *Store) Sites() ([]*Site, error) {
	rows, err := s.db.Query(`SELECT ` + siteCols + ` FROM sites ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Site
	for rows.Next() {
		site, err := scanSite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, site)
	}
	return out, rows.Err()
}

func (s *Store) Site(id int64) (*Site, error) {
	return scanSite(s.db.QueryRow(`SELECT `+siteCols+` FROM sites WHERE id = ?`, id))
}

func (s *Store) CreateSite(site *Site) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO sites (name, repo_url, branch, kind, php_version, php_extensions,
		build_assets, autorun, port, domains, env_enc, deploy_key_pub, deploy_key_enc, memory_mb, cpus, created_at, auto_deploy,
		persist_paths, redirect_aliases, cron_command, deploy_mode, stop_timeout, redirect_domains)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		site.Name, site.RepoURL, site.Branch, site.Kind, site.PHPVersion, site.PHPExtensions,
		boolInt(site.BuildAssets), boolInt(site.Autorun), site.Port, strings.Join(site.Domains, "\n"),
		site.EnvEnc, site.DeployKeyPub, site.DeployKeyEnc, site.MemoryMB, site.CPUs, time.Now().Unix(),
		boolInt(site.AutoDeploy), strings.Join(site.PersistPaths, "\n"), boolInt(site.RedirectAliases), site.CronCommand,
		deployMode(site.DeployMode), site.StopTimeout, strings.Join(site.RedirectDomains, "\n"))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateSite guarda los campos editables (el nombre cambia con RenameSite; la deploy key no cambia).
func (s *Store) UpdateSite(site *Site) error {
	_, err := s.db.Exec(`UPDATE sites SET repo_url = ?, branch = ?, kind = ?, php_version = ?, php_extensions = ?,
		build_assets = ?, autorun = ?, port = ?, domains = ?, env_enc = ?, memory_mb = ?, cpus = ?, auto_deploy = ?,
		persist_paths = ?, redirect_aliases = ?, cron_command = ?, deploy_mode = ?, stop_timeout = ?,
		redirect_domains = ? WHERE id = ?`,
		site.RepoURL, site.Branch, site.Kind, site.PHPVersion, site.PHPExtensions,
		boolInt(site.BuildAssets), boolInt(site.Autorun), site.Port, strings.Join(site.Domains, "\n"),
		site.EnvEnc, site.MemoryMB, site.CPUs, boolInt(site.AutoDeploy),
		strings.Join(site.PersistPaths, "\n"), boolInt(site.RedirectAliases), site.CronCommand,
		deployMode(site.DeployMode), site.StopTimeout, strings.Join(site.RedirectDomains, "\n"), site.ID)
	return err
}

func deployMode(m string) string {
	if m == DeployRecreate {
		return m
	}
	return DeployRolling
}

// SiteNameTaken indica si ya hay un sitio con ese nombre.
func (s *Store) SiteNameTaken(name string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM sites WHERE name = ?`, name).Scan(&n)
	return n > 0, err
}

// RenameSite cambia el nombre del sitio y le pasa su historial de métricas (guardado por nombre).
func (s *Store) RenameSite(id int64, newName string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var old string
	if err := tx.QueryRow(`SELECT name FROM sites WHERE id = ?`, id).Scan(&old); err != nil {
		return notFound(err)
	}
	if _, err := tx.Exec(`UPDATE sites SET name = ? WHERE id = ?`, newName, id); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return fmt.Errorf("ya existe un sitio llamado %s", newName)
		}
		return err
	}
	// Por si el nombre nuevo tuvo métricas antes (un sitio eliminado con ese nombre).
	if _, err := tx.Exec(`DELETE FROM metrics_ctr WHERE key = ?`, newName); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE metrics_ctr SET key = ? WHERE key = ?`, newName, old); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetSiteImage(id int64, image string) error {
	_, err := s.db.Exec(`UPDATE sites SET current_image = ? WHERE id = ?`, image, id)
	return err
}

// SetSiteCheck guarda el resultado de la última consulta a la rama remota.
func (s *Store) SetSiteCheck(id int64, at time.Time, sha, errMsg string) error {
	_, err := s.db.Exec(`UPDATE sites SET last_check_at = ?, last_remote_sha = ?, last_check_error = ? WHERE id = ?`,
		at.Unix(), sha, errMsg, id)
	return err
}

// SetSiteCron guarda el resultado de la última ejecución del cron del sitio.
func (s *Store) SetSiteCron(id int64, at time.Time, exit int, took time.Duration, output string) error {
	_, err := s.db.Exec(`UPDATE sites SET last_cron_at = ?, last_cron_exit = ?, last_cron_ms = ?, last_cron_output = ? WHERE id = ?`,
		at.Unix(), exit, took.Milliseconds(), output, id)
	return err
}

func (s *Store) DeleteSite(id int64) error {
	_, err := s.db.Exec(`DELETE FROM sites WHERE id = ?`, id)
	return err
}

// SplitLines separa texto en líneas no vacías y sin espacios sobrantes.
func SplitLines(text string) []string {
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}
