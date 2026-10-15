// Package dbmigrate copia una base MySQL/MariaDB completa hacia la base principal (MainDB):
// tablas, datos, vistas, triggers, procedimientos y eventos. El origen solo se lee.
package dbmigrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Conn son los datos de conexión a un servidor MySQL o MariaDB.
type Conn struct {
	Host     string
	Port     int
	User     string
	Password string
	Database string // vacío = sin base seleccionada
}

var (
	// Nombres de base aceptados (cPanel usa usuario_base).
	dbNameRe = regexp.MustCompile(`^[A-Za-z0-9_$-]{1,64}$`)
	// La base nueva en MainDB: solo letras, números y guion bajo (sin comillas raras en el .env).
	newNameRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)
)

func ValidDBName(s string) bool  { return dbNameRe.MatchString(s) }
func ValidNewName(s string) bool { return newNameRe.MatchString(s) }

// Bases del sistema que nunca se migran ni se usan como destino.
func systemDB(name string) bool {
	switch strings.ToLower(name) {
	case "mysql", "information_schema", "performance_schema", "sys":
		return true
	}
	return false
}

// quoteIdent pone un identificador entre comillas invertidas.
func quoteIdent(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

func open(ctx context.Context, c Conn) (*sql.DB, error) {
	cfg := mysql.NewConfig()
	cfg.User, cfg.Passwd, cfg.DBName = c.User, c.Password, c.Database
	cfg.Net, cfg.Addr = "tcp", net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	cfg.Timeout, cfg.ReadTimeout, cfg.WriteTimeout = 10*time.Second, 60*time.Second, 60*time.Second
	cfg.TLSConfig = "preferred"
	cfg.AllowNativePasswords = true
	cfg.AllowCleartextPasswords = false
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, connError(err, c)
	}
	return db, nil
}

// connError explica los errores de conexión más comunes.
func connError(err error, c Conn) error {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		switch me.Number {
		case 1045:
			return fmt.Errorf("usuario o contraseña incorrectos (o el usuario no puede entrar desde la IP de este servidor)")
		case 1044, 1049:
			return fmt.Errorf("la base %q no existe o el usuario no tiene acceso a ella", c.Database)
		case 1130:
			return fmt.Errorf("el servidor no acepta conexiones desde la IP de este servidor: agrégala en «MySQL remoto» del cPanel o en los permisos del usuario")
		}
		return fmt.Errorf("MySQL %d: %s", me.Number, me.Message)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return fmt.Errorf("no responde %s:%d (tiempo agotado): revisa el host, el puerto y el firewall del servidor de origen", c.Host, c.Port)
	}
	if strings.Contains(err.Error(), "connection refused") {
		return fmt.Errorf("%s:%d rechazó la conexión: el puerto está cerrado o MySQL no escucha conexiones remotas", c.Host, c.Port)
	}
	if strings.Contains(err.Error(), "no such host") {
		return fmt.Errorf("no existe el host %s", c.Host)
	}
	return err
}

// SourceInfo es lo que se sabe del origen antes de migrar.
type SourceInfo struct {
	Version    string // ej: 10.6.18-MariaDB
	Tables     int
	Views      int
	Routines   int // procedimientos y funciones
	Triggers   int
	Events     int
	Rows       int64 // aproximado (estadísticas del motor)
	Bytes      int64 // datos + índices, aproximado
	Charset    string
	Collation  string
	NonInnoDB  []string // tablas MyISAM/Aria: se copian, pero sin lectura consistente
	Warnings   []string
	TableNames []string
}

func (s *SourceInfo) MariaDB() bool { return strings.Contains(strings.ToLower(s.Version), "mariadb") }

// ProbeSource se conecta al origen y mide lo que se va a copiar.
func ProbeSource(ctx context.Context, c Conn) (*SourceInfo, error) {
	if systemDB(c.Database) {
		return nil, fmt.Errorf("%s es una base del sistema: no se migra", c.Database)
	}
	db, err := open(ctx, c)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	info := &SourceInfo{}
	if err := db.QueryRowContext(ctx, `SELECT VERSION()`).Scan(&info.Version); err != nil {
		return nil, err
	}
	err = db.QueryRowContext(ctx, `SELECT default_character_set_name, default_collation_name FROM information_schema.schemata WHERE schema_name = ?`,
		c.Database).Scan(&info.Charset, &info.Collation)
	if err != nil {
		return nil, fmt.Errorf("leyendo la base %s: %w", c.Database, err)
	}
	rows, err := db.QueryContext(ctx, `SELECT table_name, table_type, COALESCE(engine, ''), COALESCE(table_rows, 0),
		COALESCE(data_length, 0) + COALESCE(index_length, 0) FROM information_schema.tables WHERE table_schema = ? ORDER BY table_name`, c.Database)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var name, typ, engine string
		var n, size int64
		if err := rows.Scan(&name, &typ, &engine, &n, &size); err != nil {
			rows.Close()
			return nil, err
		}
		if typ == "VIEW" {
			info.Views++
			continue
		}
		info.Tables++
		info.Rows += n
		info.Bytes += size
		info.TableNames = append(info.TableNames, name)
		if engine != "" && !strings.EqualFold(engine, "InnoDB") {
			info.NonInnoDB = append(info.NonInnoDB, name+" ("+engine+")")
		}
	}
	rows.Close()
	count := func(q string) int {
		var n int
		db.QueryRowContext(ctx, q, c.Database).Scan(&n)
		return n
	}
	info.Routines = count(`SELECT count(*) FROM information_schema.routines WHERE routine_schema = ?`)
	info.Triggers = count(`SELECT count(*) FROM information_schema.triggers WHERE trigger_schema = ?`)
	info.Events = count(`SELECT count(*) FROM information_schema.events WHERE event_schema = ?`)

	if info.Tables == 0 {
		info.Warnings = append(info.Warnings, "La base no tiene tablas.")
	}
	if len(info.NonInnoDB) > 0 {
		info.Warnings = append(info.Warnings, fmt.Sprintf("%d tabla(s) no son InnoDB: se copian, pero si se escribe en ellas durante la copia pueden quedar desfasadas. En MainDB quedan como InnoDB.", len(info.NonInnoDB)))
	}
	if strings.Contains(info.Collation, "uca1400") {
		info.Warnings = append(info.Warnings, "La base usa una intercalación de MariaDB 11 ("+info.Collation+") que MySQL no tiene: se cambia por utf8mb4_unicode_ci.")
	}
	return info, nil
}

// TargetInfo describe MainDB para la base nueva.
type TargetInfo struct {
	Version   string
	Exists    bool // la base ya existe
	Tables    int  // tablas que ya tiene (si existe)
	BinlogFix bool // hay triggers/rutinas y el servidor no permite crearlas sin SUPER
}

// ProbeTarget revisa MainDB: conexión, versión y si el nombre nuevo está libre.
func ProbeTarget(ctx context.Context, c Conn, name string) (*TargetInfo, error) {
	if systemDB(name) {
		return nil, fmt.Errorf("%s es una base del sistema", name)
	}
	c.Database = ""
	db, err := open(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("MainDB: %w", err)
	}
	defer db.Close()
	t := &TargetInfo{}
	if err := db.QueryRowContext(ctx, `SELECT VERSION()`).Scan(&t.Version); err != nil {
		return nil, err
	}
	var n int
	db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.schemata WHERE schema_name = ?`, name).Scan(&n)
	t.Exists = n > 0
	if t.Exists {
		db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = ?`, name).Scan(&t.Tables)
	}
	var logBin, trust int
	if db.QueryRowContext(ctx, `SELECT @@log_bin, @@log_bin_trust_function_creators`).Scan(&logBin, &trust) == nil {
		t.BinlogFix = logBin == 1 && trust == 0
	}
	return t, nil
}

// TargetTables cuenta las tablas (sin vistas) que ya hay en la base nueva: es el avance de la copia.
func TargetTables(ctx context.Context, c Conn, name string) (int, error) {
	c.Database = ""
	db, err := open(ctx, c)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var n int
	err = db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = ? AND table_type = 'BASE TABLE'`, name).Scan(&n)
	return n, err
}

// CreateDatabase crea la base vacía en MainDB con el juego de caracteres del origen.
func CreateDatabase(ctx context.Context, c Conn, name, charset, collation string) error {
	charset, collation = targetCharset(charset, collation)
	c.Database = ""
	db, err := open(ctx, c)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS "+quoteIdent(name)+" CHARACTER SET "+charset+" COLLATE "+collation)
	return err
}

// DropDatabase borra la base de MainDB (solo se ofrece tras una migración fallida o cancelada).
func DropDatabase(ctx context.Context, c Conn, name string) error {
	if systemDB(name) || !ValidNewName(name) {
		return fmt.Errorf("nombre de base inválido")
	}
	c.Database = ""
	db, err := open(ctx, c)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, "DROP DATABASE IF EXISTS "+quoteIdent(name))
	return err
}

var safeIdent = regexp.MustCompile(`^[a-z0-9_]+$`)

// targetCharset adapta el juego de caracteres del origen a uno que MySQL 8 conoce.
func targetCharset(charset, collation string) (string, string) {
	charset, collation = strings.ToLower(charset), strings.ToLower(collation)
	if charset == "" || !safeIdent.MatchString(charset) || !safeIdent.MatchString(collation) {
		return "utf8mb4", "utf8mb4_unicode_ci"
	}
	if strings.Contains(collation, "uca1400") {
		collation = charset + "_unicode_ci"
	}
	if charset == "utf8" {
		charset = "utf8mb3"
		collation = strings.Replace(collation, "utf8_", "utf8mb3_", 1)
	}
	return charset, collation
}
