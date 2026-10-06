package store

import "time"

const (
	ImportRunning  = "running"
	ImportSuccess  = "success"
	ImportFailed   = "failed"
	ImportCanceled = "canceled"
)

// MailImport es una copia de correos (imapsync) desde otro servidor hacia un buzón de Stalwart.
type MailImport struct {
	ID         int64
	AccountID  string // id del buzón en Stalwart
	Email      string // buzón de destino
	Host       string // servidor de origen
	Port       int
	SrcUser    string
	Status     string
	Error      string
	Copied     int64 // mientras corre: correos revisados; al terminar: correos copiados
	Total      int64 // correos en el origen (0 = aún no se sabe)
	Bytes      int64 // bytes copiados (se conoce al terminar)
	StartedAt  time.Time
	FinishedAt time.Time // cero mientras corre
}

func (m *MailImport) Running() bool { return m.Status == ImportRunning }

func (m *MailImport) Duration() time.Duration {
	end := m.FinishedAt
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(m.StartedAt).Round(time.Second)
}

// Percent del avance (0 si todavía no se conoce el total).
func (m *MailImport) Percent() int {
	if m.Total <= 0 {
		return 0
	}
	p := int(m.Copied * 100 / m.Total)
	if p > 100 {
		p = 100
	}
	return p
}

const importCols = `id, account_id, email, host, port, src_user, status, error, copied, total, bytes, started_at, finished_at`

func scanImport(row interface{ Scan(...any) error }) (*MailImport, error) {
	var m MailImport
	var started, finished int64
	if err := row.Scan(&m.ID, &m.AccountID, &m.Email, &m.Host, &m.Port, &m.SrcUser, &m.Status, &m.Error,
		&m.Copied, &m.Total, &m.Bytes, &started, &finished); err != nil {
		return nil, notFound(err)
	}
	m.StartedAt = time.Unix(started, 0)
	if finished > 0 {
		m.FinishedAt = time.Unix(finished, 0)
	}
	return &m, nil
}

func (s *Store) CreateMailImport(m *MailImport) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO mail_imports (account_id, email, host, port, src_user, status, started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, m.AccountID, m.Email, m.Host, m.Port, m.SrcUser, ImportRunning, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) SetMailImportProgress(id, copied, total, bytes int64) error {
	_, err := s.db.Exec(`UPDATE mail_imports SET copied = ?, total = ?, bytes = ? WHERE id = ?`, copied, total, bytes, id)
	return err
}

// FinishMailImport la cierra solo si sigue corriendo (una cancelación no se pisa con el resultado del contenedor).
func (s *Store) FinishMailImport(id int64, status, errMsg string) error {
	_, err := s.db.Exec(`UPDATE mail_imports SET status = ?, error = ?, finished_at = ? WHERE id = ? AND status = ?`,
		status, errMsg, time.Now().Unix(), id, ImportRunning)
	return err
}

func (s *Store) MailImport(id int64) (*MailImport, error) {
	return scanImport(s.db.QueryRow(`SELECT `+importCols+` FROM mail_imports WHERE id = ?`, id))
}

func (s *Store) mailImports(query string, args ...any) ([]*MailImport, error) {
	rows, err := s.db.Query(`SELECT `+importCols+` FROM mail_imports `+query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*MailImport
	for rows.Next() {
		m, err := scanImport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MailImports devuelve las últimas importaciones de un buzón (la más reciente primero).
func (s *Store) MailImports(accountID string, limit int) ([]*MailImport, error) {
	return s.mailImports(`WHERE account_id = ? ORDER BY id DESC LIMIT ?`, accountID, limit)
}

// RunningMailImports: las que siguen corriendo (para retomarlas si el panel se reinicia).
func (s *Store) RunningMailImports() ([]*MailImport, error) {
	return s.mailImports(`WHERE status = ? ORDER BY id`, ImportRunning)
}
