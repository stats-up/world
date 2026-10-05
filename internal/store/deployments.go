package store

import (
	"strings"
	"time"
)

// Origen de un deploy.
const (
	SourceManual = "manual"
	SourceAuto   = "auto"
)

const (
	DeployRunning = "running"
	DeploySuccess = "success"
	DeployFailed  = "failed"
)

type Deployment struct {
	ID         int64
	SiteID     int64
	Status     string
	CommitSHA  string
	Error      string
	Source     string // manual | auto
	StartedAt  time.Time
	FinishedAt time.Time // cero mientras corre
}

func (d *Deployment) Running() bool { return d.Status == DeployRunning }

func (d *Deployment) Duration() time.Duration {
	end := d.FinishedAt
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(d.StartedAt).Round(time.Second)
}

// IsCommit indica si el deploy corresponde al commit dado (acepta SHA completo o abreviado).
func (d *Deployment) IsCommit(sha string) bool {
	if d.CommitSHA == "" || sha == "" {
		return false
	}
	return strings.HasPrefix(sha, d.CommitSHA) || strings.HasPrefix(d.CommitSHA, sha)
}

const deployCols = `id, site_id, status, commit_sha, error, source, started_at, finished_at`

func scanDeployment(row interface{ Scan(...any) error }) (*Deployment, error) {
	var d Deployment
	var started, finished int64
	if err := row.Scan(&d.ID, &d.SiteID, &d.Status, &d.CommitSHA, &d.Error, &d.Source, &started, &finished); err != nil {
		return nil, notFound(err)
	}
	d.StartedAt = time.Unix(started, 0)
	if finished > 0 {
		d.FinishedAt = time.Unix(finished, 0)
	}
	return &d, nil
}

func (s *Store) CreateDeployment(siteID int64, source string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO deployments (site_id, status, source, started_at) VALUES (?, ?, ?, ?)`,
		siteID, DeployRunning, source, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) FinishDeployment(id int64, status, commit, errMsg string) error {
	_, err := s.db.Exec(`UPDATE deployments SET status = ?, commit_sha = ?, error = ?, finished_at = ? WHERE id = ?`,
		status, commit, errMsg, time.Now().Unix(), id)
	return err
}

func (s *Store) Deployment(id int64) (*Deployment, error) {
	return scanDeployment(s.db.QueryRow(`SELECT `+deployCols+` FROM deployments WHERE id = ?`, id))
}

// LatestDeployment devuelve el último deploy del sitio (ErrNotFound si nunca se desplegó).
func (s *Store) LatestDeployment(siteID int64) (*Deployment, error) {
	return scanDeployment(s.db.QueryRow(`SELECT `+deployCols+` FROM deployments WHERE site_id = ? ORDER BY id DESC LIMIT 1`, siteID))
}

func (s *Store) Deployments(siteID int64, limit int) ([]*Deployment, error) {
	rows, err := s.db.Query(`SELECT `+deployCols+` FROM deployments WHERE site_id = ? ORDER BY id DESC LIMIT ?`, siteID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// FailInterrupted marca como fallidos los deploys que quedaron "corriendo" si el panel se reinició a mitad.
func (s *Store) FailInterrupted() error {
	_, err := s.db.Exec(`UPDATE deployments SET status = ?, error = 'interrumpido: el panel se reinició', finished_at = ?
		WHERE status = ?`, DeployFailed, time.Now().Unix(), DeployRunning)
	return err
}
