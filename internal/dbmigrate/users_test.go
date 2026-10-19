package dbmigrate

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestUserHelpers(t *testing.T) {
	if got := grantDB("client_tienda"); got != "`client\\_tienda`" {
		t.Error(got)
	}
	if n, p := unescapeDB(`client\_tienda`); n != "client_tienda" || p {
		t.Error(n, p)
	}
	if n, p := unescapeDB(`client_%`); n != "client_%" || !p {
		t.Error(n, p)
	}
	if got := quoteString(`a'b\c`); got != `'a\'b\\c'` {
		t.Error(got)
	}
	if matchProfile([]string{"SELECT", "SHOW VIEW"}) != "readonly" || matchProfile(append([]string{"GRANT"}, ProfileByKey("app").Privs...)) != "app" || matchProfile([]string{"SELECT"}) != "" {
		t.Error("matchProfile")
	}
	for i := 0; i < 50; i++ {
		p, err := GeneratePassword()
		if err != nil || len(p) != 24 || strings.ContainsAny(p, "0Ol1I#$'\" ") {
			t.Fatalf("contraseña %q %v", p, err)
		}
	}
	for _, u := range []string{"root", "rdsadmin", "mysql.sys", "mariadb.sys"} {
		if !systemUser(u) {
			t.Error(u)
		}
	}
	if systemUser("app_tienda") || ValidUserName("App") || ValidUserName("a-b") || !ValidUserName("app_tienda") {
		t.Error("nombres")
	}
}

// Prueba contra un MySQL real: WORLD_TEST_MYSQL=host:puerto:usuario:contraseña
func TestUsersMySQL(t *testing.T) {
	spec := os.Getenv("WORLD_TEST_MYSQL")
	if spec == "" {
		t.Skip("sin WORLD_TEST_MYSQL")
	}
	p := strings.SplitN(spec, ":", 4)
	port, _ := strconv.Atoi(p[1])
	admin := Conn{Host: p[0], Port: port, User: p[2], Password: p[3]}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, err := open(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{"DROP USER IF EXISTS 't_app'@'%'", "DROP DATABASE IF EXISTS t_app_db", "DROP DATABASE IF EXISTS t_other",
		"CREATE DATABASE t_app_db", "CREATE DATABASE t_other", "CREATE TABLE t_other.x (id INT)", "INSERT INTO t_other.x VALUES (1)",
		// Una base cuyo nombre calza con t_app_db si el _ fuera comodín: no debe quedar accesible.
		"DROP DATABASE IF EXISTS t1app_db", "CREATE DATABASE t1app_db"} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	defer func() {
		for _, q := range []string{"DROP USER IF EXISTS 't_app'@'%'", "DROP DATABASE IF EXISTS t_app_db", "DROP DATABASE IF EXISTS t_other", "DROP DATABASE IF EXISTS t1app_db"} {
			db.ExecContext(ctx, q)
		}
	}()
	as := func(pass string) func(q string) error {
		return func(q string) error {
			c := Conn{Host: admin.Host, Port: admin.Port, User: "t_app", Password: pass}
			conn, err := open(ctx, c)
			if err != nil {
				return err
			}
			defer conn.Close()
			_, err = conn.ExecContext(ctx, q)
			return err
		}
	}

	pass, err := CreateUser(ctx, admin, "t_app", "t_app_db", "app")
	if err != nil {
		t.Fatal(err)
	}
	run := as(pass)
	if err := run("CREATE TABLE t_app_db.a (id INT)"); err != nil {
		t.Fatalf("app debió crear tablas: %v", err)
	}
	if err := run("SELECT * FROM t_other.x"); err == nil {
		t.Fatal("no debió ver otra base")
	}
	if err := run("CREATE TABLE t1app_db.a (id INT)"); err == nil {
		t.Fatal("el _ del nombre no debe actuar como comodín")
	}
	if _, err := CreateUser(ctx, admin, "t_app", "t_app_db", "app"); err == nil || !strings.Contains(err.Error(), "ya existe") {
		t.Fatalf("duplicado: %v", err)
	}

	find := func() *DBUser {
		t.Helper()
		users, err := Users(ctx, admin)
		if err != nil {
			t.Fatal(err)
		}
		var found *DBUser
		self := false
		for i := range users {
			if users[i].User == admin.User {
				self = users[i].Self && !users[i].Editable()
			}
			if users[i].User == "t_app" {
				found = &users[i]
			}
		}
		if !self {
			t.Fatal("el administrador debe venir marcado como Self y no editable")
		}
		return found
	}
	u := find()
	if u == nil || u.Host != "%" || u.Admin() || len(u.Grants) != 1 || u.Grants[0].Name != "t_app_db" || u.Grants[0].Pattern || u.Grants[0].Profile != "app" {
		t.Fatalf("usuario: %+v", u)
	}

	// Lectura sobre otra base, y bajar la propia a solo lectura.
	if err := Grant(ctx, admin, "t_app", "%", "t_other", "readonly"); err != nil {
		t.Fatal(err)
	}
	if err := run("SELECT * FROM t_other.x"); err != nil {
		t.Fatalf("readonly debió leer: %v", err)
	}
	if err := run("INSERT INTO t_other.x VALUES (2)"); err == nil {
		t.Fatal("readonly no debió escribir")
	}
	if err := Grant(ctx, admin, "t_app", "%", "t_app_db", "data"); err != nil {
		t.Fatal(err)
	}
	if err := run("INSERT INTO t_app_db.a VALUES (1)"); err != nil {
		t.Fatalf("data debió escribir: %v", err)
	}
	if err := run("CREATE TABLE t_app_db.b (id INT)"); err == nil {
		t.Fatal("data no debió crear tablas")
	}
	u = find()
	profiles := map[string]string{}
	for _, g := range u.Grants {
		profiles[g.Name] = g.Profile
	}
	if profiles["t_app_db"] != "data" || profiles["t_other"] != "readonly" {
		t.Fatalf("perfiles: %+v", u.Grants)
	}

	// Quitar el acceso a una base (con el nombre tal como quedó guardado).
	for _, g := range u.Grants {
		if g.Name == "t_other" {
			if err := Revoke(ctx, admin, "t_app", "%", g.DB); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := run("SELECT * FROM t_other.x"); err == nil {
		t.Fatal("no debió seguir leyendo t_other")
	}

	// Contraseña nueva: la anterior deja de servir.
	pass2, err := ResetPassword(ctx, admin, "t_app", "%")
	if err != nil || pass2 == pass {
		t.Fatal(err)
	}
	if err := run("SELECT 1"); err == nil {
		t.Fatal("la contraseña anterior no debió servir")
	}
	if err := as(pass2)("SELECT * FROM t_app_db.a"); err != nil {
		t.Fatalf("contraseña nueva: %v", err)
	}

	// Cuentas protegidas.
	if err := DropUser(ctx, admin, admin.User, "%"); err == nil {
		t.Error("no debe tocar al administrador")
	}
	if _, err := ResetPassword(ctx, admin, "root", "localhost"); err == nil {
		t.Error("no debe tocar root")
	}
	if err := DropUser(ctx, admin, "t_app", "%"); err != nil {
		t.Fatal(err)
	}
	if find() != nil {
		t.Fatal("el usuario debió eliminarse")
	}
}

func TestAccessTo(t *testing.T) {
	for _, c := range []struct {
		p, s string
		ok   bool
	}{
		{`tienda`, "tienda", true}, {`tienda`, "tiendas", false},
		{`app\_db`, "app_db", true}, {`app\_db`, "appxdb", false}, {`app_db`, "appxdb", true},
		{`cliente%`, "cliente_uno", true}, {`cliente%`, "otro", false}, {`%`, "lo_que_sea", true},
	} {
		if likeMatch(c.p, c.s) != c.ok {
			t.Errorf("likeMatch(%q, %q) = %v", c.p, c.s, !c.ok)
		}
	}
	users := []DBUser{
		{User: "admin", Host: "%", Global: []string{"SELECT"}, Self: true},
		{User: "app_tienda", Host: "%", Grants: []DBGrant{{DB: `tienda`, Name: "tienda"}}},
		{User: "reportes", Host: "%", Grants: []DBGrant{{DB: `cliente%`, Name: "cliente%", Pattern: true}}},
		{User: "rdsadmin", Host: "localhost", Global: []string{"SELECT"}, System: true},
	}
	direct, global := AccessTo(users, "tienda")
	if len(direct) != 1 || direct[0].User.User != "app_tienda" || len(global) != 1 || global[0].User.User != "admin" {
		t.Errorf("tienda: direct=%v global=%v", direct, global)
	}
	if direct, _ := AccessTo(users, "cliente_uno"); len(direct) != 1 || direct[0].User.User != "reportes" {
		t.Errorf("cliente_uno: %v", direct)
	}
	m := UsersByDB(users, []string{"tienda", "vacia"})
	if len(m["tienda"]) != 1 || len(m["vacia"]) != 0 {
		t.Errorf("UsersByDB: %v", m)
	}
}
