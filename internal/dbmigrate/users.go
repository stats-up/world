package dbmigrate

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"

	"github.com/go-sql-driver/mysql"
)

// Usuarios de una instancia y sus permisos por base. La idea: un usuario por aplicación con acceso
// solo a su base, y el usuario administrador (el de la conexión) solo para administrar.

// Profile es un conjunto de permisos sobre una base.
type Profile struct {
	Key, Label, Help string
	Privs            []string
}

var Profiles = []Profile{
	{"app", "Aplicación", "Lee y escribe datos y maneja la estructura (migraciones de Laravel). El perfil para el .env de un sitio.",
		[]string{"SELECT", "INSERT", "UPDATE", "DELETE", "CREATE", "DROP", "ALTER", "INDEX", "REFERENCES",
			"CREATE TEMPORARY TABLES", "LOCK TABLES", "CREATE VIEW", "SHOW VIEW", "TRIGGER", "EXECUTE",
			"CREATE ROUTINE", "ALTER ROUTINE", "EVENT"}},
	{"data", "Solo datos", "Lee y escribe datos, pero no cambia la estructura (sin migraciones).",
		[]string{"SELECT", "INSERT", "UPDATE", "DELETE", "EXECUTE", "SHOW VIEW", "CREATE TEMPORARY TABLES", "LOCK TABLES"}},
	{"readonly", "Solo lectura", "Solo consulta: para reportes, respaldos o revisar datos.",
		[]string{"SELECT", "SHOW VIEW"}},
}

func ProfileByKey(k string) *Profile {
	for i := range Profiles {
		if Profiles[i].Key == k {
			return &Profiles[i]
		}
	}
	return nil
}

// DBGrant son los permisos de un usuario sobre una base.
type DBGrant struct {
	DB      string   // tal como se escribió en el GRANT (con \_ escapado)
	Name    string   // para mostrar
	Pattern bool     // la base tiene comodines (% o _ sin escapar): aplica a varias
	Privs   []string // ej: SELECT, INSERT
	Profile string   // clave del perfil si calza exacto
}

func (g DBGrant) ProfileLabel() string {
	if p := ProfileByKey(g.Profile); p != nil {
		return p.Label
	}
	return "Personalizado"
}

// DBUser es una cuenta de la instancia.
type DBUser struct {
	User, Host string
	Plugin     string
	Locked     bool
	Global     []string // permisos sobre todas las bases (*.*)
	Grants     []DBGrant
	Self       bool // es el usuario de la conexión (administrador)
	System     bool // cuenta interna del motor o del proveedor: no se modifica
}

func (u *DBUser) Account() string { return u.User + "@" + u.Host }

// Admin: tiene permisos globales (ve y cambia todas las bases).
func (u *DBUser) Admin() bool { return len(u.Global) > 0 }

func (u *DBUser) Editable() bool { return !u.Self && !u.System }

// systemUser: cuentas del motor (mysql.sys, mariadb.sys…) o del proveedor (rdsadmin en Lightsail/RDS).
func systemUser(name string) bool {
	n := strings.ToLower(name)
	return n == "" || n == "root" || strings.HasPrefix(n, "mysql.") || strings.HasPrefix(n, "mariadb.") ||
		strings.HasPrefix(n, "rds") || n == "healthcheck"
}

var (
	userNameRe = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)
	userHostRe = regexp.MustCompile(`^[A-Za-z0-9.%_:/-]{1,255}$`)
)

func ValidUserName(s string) bool { return userNameRe.MatchString(s) }

// quoteString arma un literal de texto de SQL.
func quoteString(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

func account(user, host string) string { return quoteString(user) + "@" + quoteString(host) }

// grantDB escribe el nombre de la base para un GRANT: _ y % son comodines, así que se escapan.
func grantDB(db string) string {
	return "`" + strings.NewReplacer("`", "``", `_`, `\_`, `%`, `\%`).Replace(db) + "`"
}

// unescapeDB pasa del nombre guardado en mysql.db al real; pattern si tiene comodines sin escapar.
func unescapeDB(s string) (name string, pattern bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\\' && i+1 < len(s):
			i++
			b.WriteByte(s[i])
		case s[i] == '%' || s[i] == '_':
			pattern = true
			b.WriteByte(s[i])
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String(), pattern
}

// rowMaps lee filas con columnas variables (mysql.user y mysql.db cambian entre versiones).
func rowMaps(rows *sql.Rows, keep func(col string) bool) ([]map[string]string, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []map[string]string
	raw := make([]sql.RawBytes, len(cols))
	ptrs := make([]any, len(cols))
	for i := range raw {
		ptrs[i] = &raw[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		m := map[string]string{}
		for i, c := range cols {
			if keep(c) {
				m[strings.ToLower(c)] = string(raw[i])
			}
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// privNames pasa de columnas Select_priv=Y a nombres de permisos (SELECT).
func privNames(m map[string]string) []string {
	var out []string
	for k, v := range m {
		if strings.HasSuffix(k, "_priv") && v == "Y" {
			p := strings.ToUpper(strings.TrimSuffix(k, "_priv"))
			out = append(out, strings.NewReplacer("CREATE_TMP_TABLE", "CREATE TEMPORARY TABLES", "_", " ").Replace(p))
		}
	}
	sort.Strings(out)
	return out
}

func matchProfile(privs []string) string {
	have := map[string]bool{}
	for _, p := range privs {
		if p != "GRANT" {
			have[p] = true
		}
	}
	for _, pr := range Profiles {
		if len(pr.Privs) != len(have) {
			continue
		}
		ok := true
		for _, p := range pr.Privs {
			ok = ok && have[p]
		}
		if ok {
			return pr.Key
		}
	}
	return ""
}

// Users lista las cuentas de la instancia con sus permisos por base (lee mysql.user y mysql.db).
func Users(ctx context.Context, c Conn) ([]DBUser, error) {
	conn, done, err := session(ctx, c)
	if err != nil {
		return nil, err
	}
	defer done()
	rows, err := conn.QueryContext(ctx, `SELECT * FROM mysql.user`)
	if err != nil {
		return nil, userError(err, c)
	}
	// Nunca se leen los hashes de contraseña.
	users, err := rowMaps(rows, func(col string) bool {
		l := strings.ToLower(col)
		return l == "user" || l == "host" || l == "plugin" || l == "account_locked" || l == "is_role" || strings.HasSuffix(l, "_priv")
	})
	rows.Close()
	if err != nil {
		return nil, err
	}
	var out []DBUser
	index := map[string]int{}
	for _, m := range users {
		if m["is_role"] == "Y" {
			continue
		}
		u := DBUser{User: m["user"], Host: m["host"], Plugin: m["plugin"], Locked: m["account_locked"] == "Y"}
		u.Global = privNames(m)
		u.Self = u.User == c.User
		u.System = systemUser(u.User)
		index[u.Account()] = len(out)
		out = append(out, u)
	}
	rows, err = conn.QueryContext(ctx, `SELECT * FROM mysql.db`)
	if err != nil {
		return nil, userError(err, c)
	}
	grants, err := rowMaps(rows, func(col string) bool {
		l := strings.ToLower(col)
		return l == "user" || l == "host" || l == "db" || strings.HasSuffix(l, "_priv")
	})
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, m := range grants {
		i, ok := index[m["user"]+"@"+m["host"]]
		if !ok {
			continue
		}
		g := DBGrant{DB: m["db"], Privs: privNames(m)}
		g.Name, g.Pattern = unescapeDB(g.DB)
		g.Profile = matchProfile(g.Privs)
		out[i].Grants = append(out[i].Grants, g)
	}
	for i := range out {
		sort.Slice(out[i].Grants, func(a, b int) bool { return out[i].Grants[a].Name < out[i].Grants[b].Name })
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].System != out[j].System {
			return !out[i].System
		}
		return out[i].Account() < out[j].Account()
	})
	return out, nil
}

// adminConn abre una conexión normal (no de solo lectura) para administrar cuentas.
func adminConn(ctx context.Context, c Conn) (*sql.DB, error) {
	c.Database = ""
	return open(ctx, c)
}

// checkTarget: solo se tocan cuentas de aplicación (ni la del administrador ni las del sistema).
func checkTarget(c Conn, user, host string) error {
	switch {
	case user == c.User:
		return errors.New("no se modifica el usuario administrador de la conexión desde aquí")
	case systemUser(user):
		return fmt.Errorf("%s es una cuenta interna del servidor: no se modifica", user)
	case !userHostRe.MatchString(host):
		return errors.New("host inválido")
	}
	return nil
}

// CreateUser crea un usuario con una contraseña generada y le da el perfil sobre la base.
// Devuelve la contraseña: world no la guarda, se muestra una sola vez.
func CreateUser(ctx context.Context, c Conn, user, db, profile string) (string, error) {
	if !ValidUserName(user) {
		return "", errors.New("nombre inválido: usa minúsculas, números y guion bajo (máx. 32)")
	}
	pr := ProfileByKey(profile)
	if pr == nil {
		return "", errors.New("perfil inválido")
	}
	if err := checkTarget(c, user, "%"); err != nil {
		return "", err
	}
	if !ValidDBName(db) || systemDB(db) {
		return "", errors.New("base inválida")
	}
	pass, err := GeneratePassword()
	if err != nil {
		return "", err
	}
	pool, err := adminConn(ctx, c)
	if err != nil {
		return "", err
	}
	defer pool.Close()
	if _, err := pool.ExecContext(ctx, "CREATE USER "+account(user, "%")+" IDENTIFIED BY "+quoteString(pass)); err != nil {
		return "", userError(err, c)
	}
	if _, err := pool.ExecContext(ctx, grantSQL(pr, db, user, "%")); err != nil {
		pool.ExecContext(ctx, "DROP USER IF EXISTS "+account(user, "%"))
		return "", userError(err, c)
	}
	return pass, nil
}

func grantSQL(pr *Profile, db, user, host string) string {
	return "GRANT " + strings.Join(pr.Privs, ", ") + " ON " + grantDB(db) + ".* TO " + account(user, host)
}

// Grant da (o reemplaza) el perfil de un usuario sobre una base.
func Grant(ctx context.Context, c Conn, user, host, db, profile string) error {
	pr := ProfileByKey(profile)
	if pr == nil {
		return errors.New("perfil inválido")
	}
	if err := checkTarget(c, user, host); err != nil {
		return err
	}
	if !ValidDBName(db) || systemDB(db) {
		return errors.New("base inválida")
	}
	pool, err := adminConn(ctx, c)
	if err != nil {
		return err
	}
	defer pool.Close()
	// Primero se quita lo que tuviera sobre esa base, para que quede exactamente el perfil.
	if _, err := pool.ExecContext(ctx, "REVOKE ALL PRIVILEGES ON "+grantDB(db)+".* FROM "+account(user, host)); err != nil {
		if !noGrant(err) {
			return userError(err, c)
		}
	}
	if _, err := pool.ExecContext(ctx, grantSQL(pr, db, user, host)); err != nil {
		return userError(err, c)
	}
	return nil
}

// Revoke quita el acceso de un usuario a una base. dbStored es el nombre tal como figura en mysql.db.
func Revoke(ctx context.Context, c Conn, user, host, dbStored string) error {
	if err := checkTarget(c, user, host); err != nil {
		return err
	}
	if dbStored == "" || strings.ContainsAny(dbStored, "`\x00") {
		return errors.New("base inválida")
	}
	pool, err := adminConn(ctx, c)
	if err != nil {
		return err
	}
	defer pool.Close()
	on := " ON `" + dbStored + "`.* FROM " + account(user, host)
	for _, q := range []string{"REVOKE ALL PRIVILEGES" + on, "REVOKE GRANT OPTION" + on} {
		if _, err := pool.ExecContext(ctx, q); err != nil && !noGrant(err) {
			return userError(err, c)
		}
	}
	return nil
}

// noGrant: el usuario no tenía ese permiso ahí (1141), no es un error al quitarlo.
func noGrant(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1141
}

// ResetPassword le pone una contraseña nueva generada y la devuelve.
func ResetPassword(ctx context.Context, c Conn, user, host string) (string, error) {
	if err := checkTarget(c, user, host); err != nil {
		return "", err
	}
	pass, err := GeneratePassword()
	if err != nil {
		return "", err
	}
	pool, err := adminConn(ctx, c)
	if err != nil {
		return "", err
	}
	defer pool.Close()
	if _, err := pool.ExecContext(ctx, "ALTER USER "+account(user, host)+" IDENTIFIED BY "+quoteString(pass)); err != nil {
		return "", userError(err, c)
	}
	return pass, nil
}

// DropUser elimina la cuenta (sus bases y datos no se tocan).
func DropUser(ctx context.Context, c Conn, user, host string) error {
	if err := checkTarget(c, user, host); err != nil {
		return err
	}
	pool, err := adminConn(ctx, c)
	if err != nil {
		return err
	}
	defer pool.Close()
	_, err = pool.ExecContext(ctx, "DROP USER "+account(user, host))
	return userError(err, c)
}

// GeneratePassword: 24 caracteres alfanuméricos (sin símbolos, para que no haya que escaparlos
// en el .env), con al menos una mayúscula, una minúscula y un número.
func GeneratePassword() (string, error) {
	const lower, upper, digits = "abcdefghijkmnopqrstuvwxyz", "ABCDEFGHJKLMNPQRSTUVWXYZ", "23456789"
	all := lower + upper + digits
	for {
		b := make([]byte, 24)
		for i := range b {
			n, err := rand.Int(rand.Reader, big.NewInt(int64(len(all))))
			if err != nil {
				return "", err
			}
			b[i] = all[n.Int64()]
		}
		s := string(b)
		if strings.ContainsAny(s, lower) && strings.ContainsAny(s, upper) && strings.ContainsAny(s, digits) {
			return s, nil
		}
	}
}

// userError explica los errores más comunes al administrar cuentas.
func userError(err error, c Conn) error {
	var me *mysql.MySQLError
	if !errors.As(err, &me) {
		return err
	}
	switch me.Number {
	case 1396: // ER_CANNOT_USER
		return errors.New("la cuenta ya existe (o ya no existe)")
	case 1044, 1045, 1142, 1227, 1410: // sin permiso
		return fmt.Errorf("el usuario %s no tiene permiso para esto (%s)", c.User, me.Message)
	case 1819: // ER_NOT_VALID_PASSWORD
		return errors.New("la contraseña generada no cumple la política del servidor (validate_password)")
	case 1470: // ER_WRONG_STRING_LENGTH
		return errors.New("el nombre es demasiado largo")
	}
	return errors.New(me.Message)
}
