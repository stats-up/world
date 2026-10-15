package store

import "time"

const (
	DBMigRunning  = "running"
	DBMigSuccess  = "success"
	DBMigFailed   = "failed"
	DBMigCanceled = "canceled"
)

// DBMigration es la copia de una base MySQL/MariaDB hacia MainDB.
type DBMigration struct {
	ID          int64
	SrcHost     string
	SrcPort     int
	SrcUser     string
	SrcDB       string
	DstDB       string
	Status      string
	Error       string
	TablesTotal int
	TablesDone  int
	BytesEst    int64 // tamaño aproximado del origen (datos + índices)
	StartedAt   time.Time
	FinishedAt  time.Time
}

func (m *DBMigration) Running() bool { return m.Status == DBMigRunning }

func (m *DBMigration) Duration() time.Duration {
	end := m.FinishedAt
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(m.StartedAt).Round(time.Second)
}

func (m *DBMigration) Percent() int {
	if m.TablesTotal <= 0 {
		return 0
	}
	return min(100, m.TablesDone*100/m.TablesTotal)
}

const dbMigCols = `id, src_host, src_port, src_user, src_db, dst_db, status, error, tables_total, tables_done, bytes_est, started_at, finished_at`

func scanDBMig(row interface{ Scan(...any) error }) (*DBMigration, error) {
	var m DBMigration
	var started, finished int64
	if err := row.Scan(&m.ID, &m.SrcHost, &m.SrcPort, &m.SrcUser, &m.SrcDB, &m.DstDB, &m.Status, &m.Error,
		&m.TablesTotal, &m.TablesDone, &m.BytesEst, &started, &finished); err != nil {
		return nil, notFound(err)
	}
	m.StartedAt = time.Unix(started, 0)
	if finished > 0 {
		m.FinishedAt = time.Unix(finished, 0)
	}
	return &m, nil
}

func (s *Store) CreateDBMigration(m *DBMigration) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO db_migrations (src_host, src_port, src_user, src_db, dst_db, status, tables_total, bytes_est, started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, m.SrcHost, m.SrcPort, m.SrcUser, m.SrcDB, m.DstDB, DBMigRunning, m.TablesTotal, m.BytesEst, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) DBMigration(id int64) (*DBMigration, error) {
	return scanDBMig(s.db.QueryRow(`SELECT `+dbMigCols+` FROM db_migrations WHERE id = ?`, id))
}

// DBMigrations: las más recientes primero.
func (s *Store) DBMigrations(limit int) ([]*DBMigration, error) {
	return s.queryDBMigs(`SELECT `+dbMigCols+` FROM db_migrations ORDER BY id DESC LIMIT ?`, limit)
}

func (s *Store) RunningDBMigrations() ([]*DBMigration, error) {
	return s.queryDBMigs(`SELECT `+dbMigCols+` FROM db_migrations WHERE status = ? ORDER BY id`, DBMigRunning)
}

// RunningDBMigrationTo: la migración en curso hacia esa base de MainDB, si hay.
func (s *Store) RunningDBMigrationTo(dst string) (*DBMigration, error) {
	return scanDBMig(s.db.QueryRow(`SELECT `+dbMigCols+` FROM db_migrations WHERE status = ? AND dst_db = ? LIMIT 1`, DBMigRunning, dst))
}

func (s *Store) queryDBMigs(q string, args ...any) ([]*DBMigration, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*DBMigration
	for rows.Next() {
		m, err := scanDBMig(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) SetDBMigrationProgress(id int64, done int) error {
	_, err := s.db.Exec(`UPDATE db_migrations SET tables_done = ? WHERE id = ? AND status = ?`, done, id, DBMigRunning)
	return err
}

// FinishDBMigration cierra la migración (solo si seguía en curso: no pisa una cancelación).
func (s *Store) FinishDBMigration(id int64, status, msg string) error {
	_, err := s.db.Exec(`UPDATE db_migrations SET status = ?, error = ?, finished_at = ? WHERE id = ? AND status = ?`,
		status, msg, time.Now().Unix(), id, DBMigRunning)
	return err
}
