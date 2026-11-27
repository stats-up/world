package store

import (
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Destinos de un backup.
const (
	BackupDisk   = "disk"
	BackupBucket = "bucket"
	BackupBoth   = "both"
)

// Estados de un backup.
const (
	BackupRunning = "running"
	BackupOK      = "ok"
	BackupFailed  = "failed"
)

const (
	MaxBackupKeep = 365
	// Días de la semana en el orden de la ISO (1 = lunes … 7 = domingo).
	AllBackupDays = "1234567"
)

// BackupConfig: si el sitio respalda su base de datos, dónde, cuándo (hora de Chile) y cuántas copias guarda.
type BackupConfig struct {
	SiteID   int64
	Enabled  bool
	Dest     string // BackupDisk | BackupBucket | BackupBoth
	BucketID int64
	Days     string // dígitos ISO: "12345" = lunes a viernes
	Hours    []int  // horas del día (0-23)
	Keep     int    // copias correctas que se conservan
}

// DefaultBackupConfig: desactivado; si se activa, a diario a las 03:00 con 7 copias en el disco.
func DefaultBackupConfig(siteID int64) *BackupConfig {
	return &BackupConfig{SiteID: siteID, Dest: BackupDisk, Days: AllBackupDays, Hours: []int{3}, Keep: 7}
}

func (c *BackupConfig) ToDisk() bool   { return c.Dest == BackupDisk || c.Dest == BackupBoth }
func (c *BackupConfig) ToBucket() bool { return c.Dest == BackupBucket || c.Dest == BackupBoth }

// HasDay indica si el día (1 = lunes … 7 = domingo) está programado.
func (c *BackupConfig) HasDay(day int) bool { return strings.Contains(c.Days, strconv.Itoa(day)) }

// Due indica si a la hora local t corresponde un backup programado.
func (c *BackupConfig) Due(t time.Time) bool {
	day := int(t.Weekday())
	if day == 0 {
		day = 7
	}
	if !c.Enabled || !c.HasDay(day) {
		return false
	}
	for _, h := range c.Hours {
		if h == t.Hour() {
			return true
		}
	}
	return false
}

// HoursText: "3, 15" para el formulario.
func (c *BackupConfig) HoursText() string {
	parts := make([]string, len(c.Hours))
	for i, h := range c.Hours {
		parts[i] = strconv.Itoa(h)
	}
	return strings.Join(parts, ", ")
}

// ParseBackupHours lee "3, 15" o "03:00,15" (horas enteras, sin repetir, en orden).
func ParseBackupHours(s string) ([]int, error) {
	seen := map[int]bool{}
	var out []int
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == ' ' }) {
		f = strings.TrimSuffix(strings.TrimSuffix(f, ":00"), "h")
		h, err := strconv.Atoi(f)
		if err != nil || h < 0 || h > 23 {
			return nil, errors.New("hora inválida: " + f + " (usa números de 0 a 23)")
		}
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("indica al menos una hora")
	}
	for i := 1; i < len(out); i++ { // pocas horas: inserción
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

func (s *Store) BackupConfig(siteID int64) (*BackupConfig, error) {
	c := DefaultBackupConfig(siteID)
	var enabled int
	var hours string
	err := s.db.QueryRow(`SELECT enabled, dest, bucket_id, days, hours, keep FROM backup_configs WHERE site_id = ?`, siteID).
		Scan(&enabled, &c.Dest, &c.BucketID, &c.Days, &hours, &c.Keep)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	c.Enabled = enabled == 1
	c.Hours, _ = ParseBackupHours(hours)
	return c, nil
}

// EnabledBackupConfigs devuelve las configuraciones activas.
func (s *Store) EnabledBackupConfigs() ([]*BackupConfig, error) {
	rows, err := s.db.Query(`SELECT site_id FROM backup_configs WHERE enabled = 1`)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	var out []*BackupConfig
	for _, id := range ids {
		c, err := s.BackupConfig(id)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func (s *Store) SaveBackupConfig(c *BackupConfig) error {
	_, err := s.db.Exec(`INSERT INTO backup_configs (site_id, enabled, dest, bucket_id, days, hours, keep) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(site_id) DO UPDATE SET enabled = excluded.enabled, dest = excluded.dest, bucket_id = excluded.bucket_id,
		days = excluded.days, hours = excluded.hours, keep = excluded.keep`,
		c.SiteID, boolInt(c.Enabled), c.Dest, c.BucketID, c.Days, c.HoursText(), c.Keep)
	return err
}

// Backup es una copia de la base de datos de un sitio.
type Backup struct {
	ID           int64
	SiteID       int64
	SiteName     string
	DBName       string
	Source       string // auto | manual
	Status       string
	Error        string
	Bytes        int64
	File         string // nombre del archivo .sql.gz
	OnDisk       bool
	BucketID     int64
	BucketKey    string
	StartedAt    time.Time
	FinishedAt   time.Time
	RestoredAt   time.Time
	RestoreError string
}

func (b *Backup) Running() bool { return b.Status == BackupRunning }
func (b *Backup) OK() bool      { return b.Status == BackupOK }

// Duration: cuánto tardó (vacío si sigue corriendo).
func (b *Backup) Duration() string {
	if b.FinishedAt.IsZero() {
		return ""
	}
	return b.FinishedAt.Sub(b.StartedAt).Round(time.Second).String()
}

const backupCols = `id, site_id, site_name, db_name, source, status, error, bytes, file, on_disk, bucket_id, bucket_key,
	started_at, finished_at, restored_at, restore_error`

func scanBackup(row interface{ Scan(...any) error }) (*Backup, error) {
	var b Backup
	var onDisk int
	var started, finished, restored int64
	err := row.Scan(&b.ID, &b.SiteID, &b.SiteName, &b.DBName, &b.Source, &b.Status, &b.Error, &b.Bytes, &b.File, &onDisk,
		&b.BucketID, &b.BucketKey, &started, &finished, &restored, &b.RestoreError)
	if err != nil {
		return nil, notFound(err)
	}
	b.OnDisk = onDisk == 1
	b.StartedAt = time.Unix(started, 0)
	if finished > 0 {
		b.FinishedAt = time.Unix(finished, 0)
	}
	if restored > 0 {
		b.RestoredAt = time.Unix(restored, 0)
	}
	return &b, nil
}

func (s *Store) CreateBackup(b *Backup) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO backups (site_id, site_name, db_name, source, status, started_at) VALUES (?, ?, ?, ?, ?, ?)`,
		b.SiteID, b.SiteName, b.DBName, b.Source, BackupRunning, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) Backup(id int64) (*Backup, error) {
	return scanBackup(s.db.QueryRow(`SELECT `+backupCols+` FROM backups WHERE id = ?`, id))
}

// Backups del sitio, del más nuevo al más antiguo.
func (s *Store) Backups(siteID int64, limit int) ([]*Backup, error) {
	rows, err := s.db.Query(`SELECT `+backupCols+` FROM backups WHERE site_id = ? ORDER BY started_at DESC, id DESC LIMIT ?`, siteID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Backup
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// LastAutoBackup: inicio del último backup programado del sitio (cero si nunca hubo).
func (s *Store) LastAutoBackup(siteID int64) time.Time {
	var t sql.NullInt64
	s.db.QueryRow(`SELECT MAX(started_at) FROM backups WHERE site_id = ? AND source = 'auto'`, siteID).Scan(&t)
	if !t.Valid {
		return time.Time{}
	}
	return time.Unix(t.Int64, 0)
}

// FinishBackup guarda el resultado y dónde quedó el archivo.
func (s *Store) FinishBackup(b *Backup) error {
	_, err := s.db.Exec(`UPDATE backups SET status = ?, error = ?, bytes = ?, file = ?, on_disk = ?, bucket_id = ?, bucket_key = ?,
		finished_at = ? WHERE id = ?`, b.Status, b.Error, b.Bytes, b.File, boolInt(b.OnDisk), b.BucketID, b.BucketKey, time.Now().Unix(), b.ID)
	return err
}

// SetBackupRestored anota el resultado de una restauración (errMsg vacío = correcta).
func (s *Store) SetBackupRestored(id int64, errMsg string) error {
	_, err := s.db.Exec(`UPDATE backups SET restored_at = ?, restore_error = ? WHERE id = ?`, time.Now().Unix(), errMsg, id)
	return err
}

func (s *Store) DeleteBackup(id int64) error {
	_, err := s.db.Exec(`DELETE FROM backups WHERE id = ?`, id)
	return err
}

// FailInterruptedBackups marca como fallidos los backups que corrían cuando el panel se detuvo.
func (s *Store) FailInterruptedBackups() error {
	_, err := s.db.Exec(`UPDATE backups SET status = ?, error = 'interrumpido: el panel se reinició', finished_at = ? WHERE status = ?`,
		BackupFailed, time.Now().Unix(), BackupRunning)
	return err
}

// DeleteSiteBackups borra la configuración y los registros de backups del sitio.
func (s *Store) DeleteSiteBackups(siteID int64) error {
	if _, err := s.db.Exec(`DELETE FROM backups WHERE site_id = ?`, siteID); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM backup_configs WHERE site_id = ?`, siteID)
	return err
}
