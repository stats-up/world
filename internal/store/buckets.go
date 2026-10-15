package store

import (
	"database/sql"
	"time"
)

const (
	ScanRunning = "running"
	ScanOK      = "ok"
	ScanFailed  = "failed"
)

// Bucket es un bucket S3 que world mide (solo lectura). La llave secreta va cifrada.
type Bucket struct {
	ID           int64
	Name         string // nombre para mostrar
	Bucket       string
	Region       string
	Endpoint     string // vacío = AWS/Lightsail
	AccessKey    string
	SecretEnc    []byte
	CreatedAt    time.Time
	ScanStatus   string // "" (nunca) | running | ok | failed
	ScanError    string
	ScannedAt    time.Time // fin del último escaneo exitoso
	ScanMS       int64
	TotalBytes   int64
	TotalObjects int64
	Truncated    bool // demasiadas carpetas: las más profundas se sumaron a su carpeta padre
}

func (b *Bucket) Scanning() bool { return b.ScanStatus == ScanRunning }

// BucketFolder: tamaño de una carpeta (prefijo) en el último escaneo.
type BucketFolder struct {
	Prefix    string
	Bytes     int64
	Objects   int64
	FileBytes int64
	Files     int64
}

const bucketCols = `id, name, bucket, region, endpoint, access_key, secret_enc, created_at, scan_status, scan_error,
	scanned_at, scan_ms, total_bytes, total_objects, truncated`

func scanBucket(row interface{ Scan(...any) error }) (*Bucket, error) {
	var b Bucket
	var created, scanned int64
	var truncated int
	if err := row.Scan(&b.ID, &b.Name, &b.Bucket, &b.Region, &b.Endpoint, &b.AccessKey, &b.SecretEnc, &created,
		&b.ScanStatus, &b.ScanError, &scanned, &b.ScanMS, &b.TotalBytes, &b.TotalObjects, &truncated); err != nil {
		return nil, notFound(err)
	}
	b.CreatedAt = time.Unix(created, 0)
	if scanned > 0 {
		b.ScannedAt = time.Unix(scanned, 0)
	}
	b.Truncated = truncated == 1
	return &b, nil
}

func (s *Store) CreateBucket(b *Bucket) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO buckets (name, bucket, region, endpoint, access_key, secret_enc, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, b.Name, b.Bucket, b.Region, b.Endpoint, b.AccessKey, b.SecretEnc, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) Bucket(id int64) (*Bucket, error) {
	return scanBucket(s.db.QueryRow(`SELECT `+bucketCols+` FROM buckets WHERE id = ?`, id))
}

// Buckets ordenados por espacio usado, de mayor a menor.
func (s *Store) Buckets() ([]*Bucket, error) {
	rows, err := s.db.Query(`SELECT ` + bucketCols + ` FROM buckets ORDER BY total_bytes DESC, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Bucket
	for rows.Next() {
		b, err := scanBucket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) DeleteBucket(id int64) error {
	_, err := s.db.Exec(`DELETE FROM buckets WHERE id = ?`, id) // las carpetas se borran en cascada
	return err
}

func (s *Store) StartBucketScan(id int64) error {
	_, err := s.db.Exec(`UPDATE buckets SET scan_status = ?, scan_error = '' WHERE id = ?`, ScanRunning, id)
	return err
}

func (s *Store) FailBucketScan(id int64, msg string) error {
	_, err := s.db.Exec(`UPDATE buckets SET scan_status = ?, scan_error = ? WHERE id = ?`, ScanFailed, msg, id)
	return err
}

// InterruptedBucketScans marca como fallidos los escaneos que quedaron "en curso" al reiniciar el panel.
func (s *Store) InterruptedBucketScans() error {
	_, err := s.db.Exec(`UPDATE buckets SET scan_status = ?, scan_error = 'se interrumpió al reiniciar el panel'
		WHERE scan_status = ?`, ScanFailed, ScanRunning)
	return err
}

// FinishBucketScan reemplaza las carpetas del bucket por las del escaneo nuevo.
func (s *Store) FinishBucketScan(id int64, folders []BucketFolder, root BucketFolder, truncated bool, took time.Duration) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM bucket_folders WHERE bucket_id = ?`, id); err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO bucket_folders (bucket_id, prefix, parent, bytes, objects, file_bytes, files)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, f := range folders {
		if _, err := stmt.Exec(id, f.Prefix, parentPrefix(f.Prefix), f.Bytes, f.Objects, f.FileBytes, f.Files); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE buckets SET scan_status = ?, scan_error = '', scanned_at = ?, scan_ms = ?,
		total_bytes = ?, total_objects = ?, truncated = ? WHERE id = ?`,
		ScanOK, time.Now().Unix(), took.Milliseconds(), root.Bytes, root.Objects, boolInt(truncated), id); err != nil {
		return err
	}
	return tx.Commit()
}

// parentPrefix: "a/b/" → "a/", "a/" → "", "" → "" (la raíz no tiene padre; se guarda con parent '-').
func parentPrefix(prefix string) string {
	if prefix == "" {
		return "-"
	}
	p := prefix[:len(prefix)-1]
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i+1]
		}
	}
	return ""
}

func (s *Store) BucketFolder(id int64, prefix string) (*BucketFolder, error) {
	var f BucketFolder
	err := s.db.QueryRow(`SELECT prefix, bytes, objects, file_bytes, files FROM bucket_folders WHERE bucket_id = ? AND prefix = ?`,
		id, prefix).Scan(&f.Prefix, &f.Bytes, &f.Objects, &f.FileBytes, &f.Files)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return &f, err
}

// BucketSubfolders: las subcarpetas directas de prefix, de mayor a menor espacio usado.
func (s *Store) BucketSubfolders(id int64, prefix string, limit int) ([]BucketFolder, int, error) {
	var total int
	if err := s.db.QueryRow(`SELECT count(*) FROM bucket_folders WHERE bucket_id = ? AND parent = ?`, id, prefix).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(`SELECT prefix, bytes, objects, file_bytes, files FROM bucket_folders
		WHERE bucket_id = ? AND parent = ? ORDER BY bytes DESC, prefix LIMIT ?`, id, prefix, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []BucketFolder
	for rows.Next() {
		var f BucketFolder
		if err := rows.Scan(&f.Prefix, &f.Bytes, &f.Objects, &f.FileBytes, &f.Files); err != nil {
			return nil, 0, err
		}
		out = append(out, f)
	}
	return out, total, rows.Err()
}
