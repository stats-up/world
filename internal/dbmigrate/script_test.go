package dbmigrate

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSplitScript(t *testing.T) {
	script := "-- encabezado; con punto y coma\n" +
		"SELECT 'a;b', \"c;d\", `e;f` FROM t; # comentario; final\n" +
		"UPDATE users SET name = 'O\\'Brien; x' WHERE id = 1;\n" +
		"/* bloque; */ INSERT INTO t VALUES ('it''s');\n" +
		"DELIMITER $$\n" +
		"CREATE PROCEDURE p()\nBEGIN\n  SELECT 1;\n  SELECT 2;\nEND$$\n" +
		"DELIMITER ;\n" +
		"/*!40101 SET NAMES utf8mb4 */;\n" +
		"delete from logs;\n" +
		"DROP TABLE IF EXISTS viejo\n"
	stmts, err := SplitScript(script)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, s := range stmts {
		kinds = append(kinds, s.Kind)
	}
	want := "SELECT|UPDATE|INSERT|CREATE PROCEDURE|SET|DELETE|DROP TABLE"
	if got := strings.Join(kinds, "|"); got != want {
		t.Fatalf("tipos:\n got %s\nwant %s\n%#v", got, want, stmts)
	}
	if !strings.Contains(stmts[3].SQL, "SELECT 1;\n  SELECT 2;") || !strings.HasSuffix(stmts[3].SQL, "END") {
		t.Errorf("procedimiento: %q", stmts[3].SQL)
	}
	if stmts[1].Line != 3 || stmts[3].Line != 6 || stmts[6].Line != 14 {
		t.Errorf("líneas: %d %d %d", stmts[1].Line, stmts[3].Line, stmts[6].Line)
	}
	if !stmts[0].Read() || stmts[1].Read() || !stmts[4].Read() {
		t.Error("clasificación de lectura")
	}
	if stmts[1].Danger() != "" || stmts[5].Danger() == "" || stmts[6].Danger() != "DROP TABLE" {
		t.Errorf("peligros: %q %q %q", stmts[1].Danger(), stmts[5].Danger(), stmts[6].Danger())
	}
	// Un WHERE dentro de un texto no cuenta.
	if s, _ := SplitScript("UPDATE t SET note = 'sin where aqui WHERE'"); s[0].Danger() == "" {
		t.Error("WHERE entre comillas no debe contar")
	}
	p := Plan(stmts)
	if p.Writes != 5 || p.Summary() != "1 UPDATE, 1 INSERT, 1 CREATE PROCEDURE, 1 DELETE, 1 DROP TABLE" || len(p.Dangers) != 2 {
		t.Errorf("plan: %d %q %v", p.Writes, p.Summary(), p.Dangers)
	}
	for _, bad := range []string{"SELECT 'abierto", "SELECT 1 /* sin cerrar", "DELIMITER \nSELECT 1"} {
		if _, err := SplitScript(bad); err == nil {
			t.Errorf("debió fallar: %q", bad)
		}
	}
	if s, _ := SplitScript("-- solo comentario\n  ;  ;"); len(s) != 0 {
		t.Errorf("solo comentarios: %#v", s)
	}
	if s, _ := SplitScript("SELECT * FROM t INTO OUTFILE '/tmp/x'"); s[0].Read() {
		t.Error("INTO OUTFILE escribe archivos")
	}
}

func TestScriptMySQL(t *testing.T) {
	spec := os.Getenv("WORLD_TEST_MYSQL")
	if spec == "" {
		t.Skip("sin WORLD_TEST_MYSQL")
	}
	p := strings.SplitN(spec, ":", 4)
	port, _ := strconv.Atoi(p[1])
	c := Conn{Host: p[0], Port: port, User: p[2], Password: p[3]}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	run := func(script, db string, tx bool) *ScriptRun {
		t.Helper()
		stmts, err := SplitScript(script)
		if err != nil {
			t.Fatal(err)
		}
		r, err := RunScript(ctx, c, stmts, ScriptOptions{DB: db, Transaction: tx})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := run("DROP DATABASE IF EXISTS t_script; CREATE DATABASE t_script; USE t_script;\n"+
		"CREATE TABLE t (id INT AUTO_INCREMENT PRIMARY KEY, name VARCHAR(20), bin VARBINARY(4));\n"+
		"INSERT INTO t (name, bin) VALUES ('uno', 0x01020304), ('dos', NULL), ('tres;x', NULL);\n"+
		"DELIMITER $$\nCREATE PROCEDURE p() BEGIN SELECT COUNT(*) AS n FROM t; SELECT 'otro'; END$$\nDELIMITER ;\n", "", false)
	if r.Failed() {
		t.Fatalf("preparar: %+v", r.Results[len(r.Results)-1])
	}
	if ins := r.Results[4]; ins.Affected != 3 || ins.InsertID != 1 {
		t.Errorf("insert: %+v", ins)
	}

	// Lecturas: filas, binarios, NULL y CALL con varios resultados.
	r = run("SELECT id, name, bin FROM t ORDER BY id; CALL p(); SHOW TABLES;", "t_script", false)
	if r.Failed() {
		t.Fatal(r.Results[len(r.Results)-1].Err)
	}
	sel := r.Results[0]
	if sel.Total != 3 || len(sel.Columns) != 3 || !sel.Rows[0][2].Binary || sel.Rows[0][2].Size != 4 || !sel.Rows[1][2].Null || sel.Rows[2][1].Text != "tres;x" {
		t.Errorf("select: %+v", sel)
	}
	if call := r.Results[1]; call.Sets < 2 || call.Rows[0][0].Text != "3" {
		t.Errorf("call: %+v", call)
	}

	// Transacción: si una falla, los cambios de datos anteriores se deshacen.
	r = run("UPDATE t SET name = 'cambiado' WHERE id = 1; INSERT INTO no_existe VALUES (1); DELETE FROM t;", "t_script", true)
	if !r.Failed() || !r.RolledBack || r.Skipped != 1 || r.FailedLine() != 1 {
		t.Errorf("rollback: %+v", r)
	}
	if r = run("SELECT name FROM t WHERE id = 1", "t_script", false); r.Results[0].Rows[0][0].Text != "uno" {
		t.Errorf("el UPDATE debió deshacerse: %+v", r.Results[0].Rows)
	}
	// Sin transacción lo anterior al error queda aplicado.
	run("UPDATE t SET name = 'cambiado' WHERE id = 1; INSERT INTO no_existe VALUES (1);", "t_script", false)
	if r = run("SELECT name FROM t WHERE id = 1", "t_script", false); r.Results[0].Rows[0][0].Text != "cambiado" {
		t.Errorf("sin transacción debió quedar: %+v", r.Results[0].Rows)
	}
	// Transacción exitosa.
	if r = run("UPDATE t SET name = 'x' WHERE id = 2; DELETE FROM t WHERE id = 3;", "t_script", true); !r.Committed || r.Results[1].Affected != 1 {
		t.Errorf("commit: %+v", r)
	}
	run("DROP DATABASE t_script", "", false)
}
