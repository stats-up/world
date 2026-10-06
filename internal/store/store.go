// Package store guarda el estado del panel en SQLite.
package store

import (
	"database/sql"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("no encontrado")

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Una sola conexión: evita SQLITE_BUSY y la carga del panel es baja.
	// Consecuencia: nunca hacer una consulta mientras se itera otra (rows abiertos).
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrando base de datos: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Cada elemento es una versión del esquema; nunca editar una ya publicada, solo agregar al final.
var migrations = []string{
	`CREATE TABLE users (
		id            INTEGER PRIMARY KEY,
		email         TEXT NOT NULL UNIQUE,
		password_hash TEXT NOT NULL,
		totp_secret   TEXT NOT NULL DEFAULT '',
		totp_enabled  INTEGER NOT NULL DEFAULT 0,
		created_at    INTEGER NOT NULL
	);
	CREATE TABLE sessions (
		token_hash  TEXT PRIMARY KEY,
		user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		csrf        TEXT NOT NULL,
		mfa_pending INTEGER NOT NULL DEFAULT 0,
		expires_at  INTEGER NOT NULL
	);
	CREATE TABLE settings (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);
	CREATE TABLE sites (
		id             INTEGER PRIMARY KEY,
		name           TEXT NOT NULL UNIQUE,
		repo_url       TEXT NOT NULL,
		branch         TEXT NOT NULL DEFAULT 'main',
		kind           TEXT NOT NULL DEFAULT 'php',
		php_version    TEXT NOT NULL DEFAULT '8.3',
		php_extensions TEXT NOT NULL DEFAULT '',
		build_assets   INTEGER NOT NULL DEFAULT 1,
		autorun        INTEGER NOT NULL DEFAULT 0,
		port           INTEGER NOT NULL DEFAULT 8080,
		domains        TEXT NOT NULL DEFAULT '',
		env_enc        BLOB,
		deploy_key_pub TEXT NOT NULL DEFAULT '',
		deploy_key_enc BLOB,
		memory_mb      INTEGER NOT NULL DEFAULT 0,
		cpus           REAL NOT NULL DEFAULT 0,
		current_image  TEXT NOT NULL DEFAULT '',
		created_at     INTEGER NOT NULL
	);
	CREATE TABLE deployments (
		id          INTEGER PRIMARY KEY,
		site_id     INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		status      TEXT NOT NULL,
		commit_sha  TEXT NOT NULL DEFAULT '',
		error       TEXT NOT NULL DEFAULT '',
		started_at  INTEGER NOT NULL,
		finished_at INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX deployments_site ON deployments(site_id, id DESC);`,

	// 2: auto-deploy (world revisa la rama cada minuto y despliega si hay commits nuevos)
	`ALTER TABLE sites ADD COLUMN auto_deploy INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE sites ADD COLUMN last_check_at INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE sites ADD COLUMN last_remote_sha TEXT NOT NULL DEFAULT '';
	ALTER TABLE sites ADD COLUMN last_check_error TEXT NOT NULL DEFAULT '';
	ALTER TABLE deployments ADD COLUMN source TEXT NOT NULL DEFAULT 'manual';`,

	// 3: carpetas persistentes, cron por sitio (ej: schedule:run) y redirección de alias al dominio principal
	`ALTER TABLE sites ADD COLUMN persist_paths TEXT NOT NULL DEFAULT '';
	ALTER TABLE sites ADD COLUMN cron_command TEXT NOT NULL DEFAULT '';
	ALTER TABLE sites ADD COLUMN redirect_aliases INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE sites ADD COLUMN last_cron_at INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE sites ADD COLUMN last_cron_exit INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE sites ADD COLUMN last_cron_ms INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE sites ADD COLUMN last_cron_output TEXT NOT NULL DEFAULT '';`,

	// 4: monitoreo. res = segundos que cubre cada fila (60 = detalle, 900 = resumen de 15 min).
	`CREATE TABLE metrics_host (
		res        INTEGER NOT NULL,
		ts         INTEGER NOT NULL,
		cpu        REAL    NOT NULL,
		mem_used   INTEGER NOT NULL,
		mem_total  INTEGER NOT NULL,
		swap_used  INTEGER NOT NULL,
		swap_total INTEGER NOT NULL,
		disk_used  INTEGER NOT NULL,
		disk_total INTEGER NOT NULL,
		load1      REAL    NOT NULL,
		PRIMARY KEY (res, ts)
	) WITHOUT ROWID;
	CREATE TABLE metrics_ctr (
		key       TEXT    NOT NULL,
		res       INTEGER NOT NULL,
		ts        INTEGER NOT NULL,
		cpu       REAL    NOT NULL,
		mem       INTEGER NOT NULL,
		mem_limit INTEGER NOT NULL,
		net_rx    REAL    NOT NULL,
		net_tx    REAL    NOT NULL,
		blk_r     REAL    NOT NULL,
		blk_w     REAL    NOT NULL,
		restarts  INTEGER NOT NULL,
		oom       INTEGER NOT NULL,
		PRIMARY KEY (key, res, ts)
	) WITHOUT ROWID;
	CREATE INDEX metrics_ctr_ts ON metrics_ctr (res, ts);`,
}

func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migración %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Setting(key string) string {
	var v string
	s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	return v
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
