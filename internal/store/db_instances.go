package store

import "time"

// DBInstance es un servidor MySQL/MariaDB que se puede explorar (además de MainDB).
// La contraseña va cifrada.
type DBInstance struct {
	ID        int64
	Name      string
	Host      string
	Port      int
	User      string
	PassEnc   []byte
	CreatedAt time.Time
}

const dbInstanceCols = `id, name, host, port, user, pass_enc, created_at`

func scanDBInstance(row interface{ Scan(...any) error }) (*DBInstance, error) {
	var d DBInstance
	var created int64
	if err := row.Scan(&d.ID, &d.Name, &d.Host, &d.Port, &d.User, &d.PassEnc, &created); err != nil {
		return nil, notFound(err)
	}
	d.CreatedAt = time.Unix(created, 0)
	return &d, nil
}

func (s *Store) CreateDBInstance(d *DBInstance) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO db_instances (name, host, port, user, pass_enc, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		d.Name, d.Host, d.Port, d.User, d.PassEnc, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) DBInstance(id int64) (*DBInstance, error) {
	return scanDBInstance(s.db.QueryRow(`SELECT `+dbInstanceCols+` FROM db_instances WHERE id = ?`, id))
}

func (s *Store) DBInstances() ([]*DBInstance, error) {
	rows, err := s.db.Query(`SELECT ` + dbInstanceCols + ` FROM db_instances ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*DBInstance
	for rows.Next() {
		d, err := scanDBInstance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) DeleteDBInstance(id int64) error {
	_, err := s.db.Exec(`DELETE FROM db_instances WHERE id = ?`, id)
	return err
}
