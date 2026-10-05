package store

import (
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
	auto_deploy, last_check_at, last_remote_sha, last_check_error`

func scanSite(row interface{ Scan(...any) error }) (*Site, error) {
	var s Site
	var assets, autorun, auto int
	var domains string
	var created, checked int64
	err := row.Scan(&s.ID, &s.Name, &s.RepoURL, &s.Branch, &s.Kind, &s.PHPVersion, &s.PHPExtensions,
		&assets, &autorun, &s.Port, &domains, &s.EnvEnc, &s.DeployKeyPub, &s.DeployKeyEnc,
		&s.MemoryMB, &s.CPUs, &s.CurrentImage, &created,
		&auto, &checked, &s.LastRemoteSHA, &s.LastCheckError)
	if err != nil {
		return nil, notFound(err)
	}
	s.BuildAssets = assets == 1
	s.Autorun = autorun == 1
	s.AutoDeploy = auto == 1
	s.Domains = SplitLines(domains)
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
		build_assets, autorun, port, domains, env_enc, deploy_key_pub, deploy_key_enc, memory_mb, cpus, created_at, auto_deploy)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		site.Name, site.RepoURL, site.Branch, site.Kind, site.PHPVersion, site.PHPExtensions,
		boolInt(site.BuildAssets), boolInt(site.Autorun), site.Port, strings.Join(site.Domains, "\n"),
		site.EnvEnc, site.DeployKeyPub, site.DeployKeyEnc, site.MemoryMB, site.CPUs, time.Now().Unix(),
		boolInt(site.AutoDeploy))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateSite guarda los campos editables (el nombre y la deploy key no cambian).
func (s *Store) UpdateSite(site *Site) error {
	_, err := s.db.Exec(`UPDATE sites SET repo_url = ?, branch = ?, kind = ?, php_version = ?, php_extensions = ?,
		build_assets = ?, autorun = ?, port = ?, domains = ?, env_enc = ?, memory_mb = ?, cpus = ?, auto_deploy = ?
		WHERE id = ?`,
		site.RepoURL, site.Branch, site.Kind, site.PHPVersion, site.PHPExtensions,
		boolInt(site.BuildAssets), boolInt(site.Autorun), site.Port, strings.Join(site.Domains, "\n"),
		site.EnvEnc, site.MemoryMB, site.CPUs, boolInt(site.AutoDeploy), site.ID)
	return err
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
