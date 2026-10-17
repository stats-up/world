package dbmigrate

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTextCellAndLike(t *testing.T) {
	if c := textCell(strings.Repeat("ñ", cellMax+5)); !c.Cut || len([]rune(c.Text)) != cellMax {
		t.Errorf("recorte: %d %v", len([]rune(c.Text)), c.Cut)
	}
	if c := textCell("hola"); c.Cut || c.Text != "hola" {
		t.Error(c)
	}
	if got := escapeLike(`50%_a\b`); got != `50\%\_a\\b` {
		t.Error(got)
	}
}

// Prueba contra un MySQL real: WORLD_TEST_MYSQL=host:puerto:usuario:contraseña
func TestInspectMySQL(t *testing.T) {
	spec := os.Getenv("WORLD_TEST_MYSQL")
	if spec == "" {
		t.Skip("sin WORLD_TEST_MYSQL")
	}
	p := strings.SplitN(spec, ":", 4)
	port, _ := strconv.Atoi(p[1])
	c := Conn{Host: p[0], Port: port, User: p[2], Password: p[3]}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	db, err := open(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"DROP DATABASE IF EXISTS `t-explora`",
		"CREATE DATABASE `t-explora` CHARACTER SET latin1",
		"CREATE TABLE `t-explora`.`gente` (id INT PRIMARY KEY AUTO_INCREMENT, nombre VARCHAR(50), nota TEXT, foto BLOB, KEY ix_nombre (nombre)) COMMENT 'personas'",
		"INSERT INTO `t-explora`.`gente` (nombre, nota, foto) VALUES ('José', REPEAT('x', 400), UNHEX('FFD8FFE0')), ('50%_off', NULL, NULL), ('Ana', 'hola', NULL)",
		"CREATE VIEW `t-explora`.`v_gente` AS SELECT id, nombre FROM `t-explora`.`gente`",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	db.Close()

	sum, err := Summary(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	var found *DBSummary
	for i := range sum.Databases {
		if sum.Databases[i].Name == "t-explora" {
			found = &sum.Databases[i]
		}
	}
	if found == nil || found.Tables != 1 || found.Views != 1 || found.System {
		t.Fatalf("summary: %+v", found)
	}
	tables, err := Tables(ctx, c, "t-explora")
	if err != nil || len(tables) != 2 || tables[0].Name != "gente" || tables[0].Comment != "personas" || !tables[1].View() {
		t.Fatalf("tables: %+v %v", tables, err)
	}
	if _, err := Tables(ctx, c, "no-existe"); err != ErrNotFound {
		t.Fatal(err)
	}
	td, err := Table(ctx, c, "t-explora", "gente")
	if err != nil || len(td.Columns) != 4 || len(td.Indexes) != 2 || td.Indexes[0].Name != "PRIMARY" || !strings.Contains(td.Create, "CREATE TABLE") {
		t.Fatalf("table: %+v %v", td, err)
	}
	if v, err := Table(ctx, c, "t-explora", "v_gente"); err != nil || !strings.Contains(v.Create, "VIEW") {
		t.Fatalf("view: %+v %v", v, err)
	}
	_, rows, more, err := Rows(ctx, c, "t-explora", "gente", RowQuery{Limit: 2, Order: "id"})
	if err != nil || len(rows) != 2 || !more {
		t.Fatalf("rows: %v %v %v", rows, more, err)
	}
	if r := rows[0].Cells; r[1].Text != "José" || !r[2].Cut || len(r[2].Text) != cellMax || !r[3].Binary || r[3].Size != 4 {
		t.Fatalf("fila 1: %+v", r)
	}
	if r := rows[1].Cells; !r[2].Null || !r[3].Null {
		t.Fatalf("fila 2: %+v", r)
	}
	// El filtro escapa % y _ (busca el texto literal), y una columna desconocida se ignora.
	_, rows, _, err = Rows(ctx, c, "t-explora", "gente", RowQuery{Limit: 50, FilterCol: "nombre", FilterVal: "%_"})
	if err != nil || len(rows) != 1 || rows[0].Cells[1].Text != "50%_off" {
		t.Fatalf("filtro: %v %v", rows, err)
	}
	_, rows, _, err = Rows(ctx, c, "t-explora", "gente", RowQuery{Limit: 50, Order: "id`; DROP TABLE gente; --", Desc: true, FilterCol: "x", FilterVal: "y"})
	if err != nil || len(rows) != 3 {
		t.Fatalf("columnas inválidas: %v %v", rows, err)
	}
	_, rows, _, err = Rows(ctx, c, "t-explora", "gente", RowQuery{Limit: 1, Offset: 2, Order: "nombre", Desc: true})
	if err != nil || len(rows) != 1 || rows[0].Cells[1].Text != "50%_off" { // José, Ana, 50%_off
		t.Fatalf("orden/página: %v %v", rows, err)
	}
	if _, _, _, err := Rows(ctx, c, "t-explora", "no-existe", RowQuery{}); err != ErrNotFound {
		t.Fatal(err)
	}
	_, rows, _, _ = Rows(ctx, c, "t-explora", "gente", RowQuery{Limit: 1, Order: "id"})
	if rows[0].Key != `["1"]` || rows[0].Label != "id = 1" {
		t.Fatalf("clave: %q %q", rows[0].Key, rows[0].Label)
	}

	// Eliminar: por clave, en transacción, explicando las claves foráneas.
	db, err = open(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"CREATE TABLE `t-explora`.`hijos` (id INT PRIMARY KEY, gente_id INT, FOREIGN KEY (gente_id) REFERENCES `t-explora`.`gente`(id))",
		"INSERT INTO `t-explora`.`hijos` VALUES (1, 1)",
		"CREATE TABLE `t-explora`.`sinclave` (a INT, b INT)",
		"CREATE TABLE `t-explora`.`bin` (u BINARY(2) NOT NULL, x INT, UNIQUE KEY (u))",
		"INSERT INTO `t-explora`.`bin` VALUES (UNHEX('00FF'), 1), (UNHEX('0100'), 2)",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	count := func(q string) (n int) {
		db.QueryRowContext(ctx, q).Scan(&n)
		return
	}
	defer db.Close()
	if td, _ := Table(ctx, c, "t-explora", "gente"); len(td.References) != 1 || td.References[0].Table != "hijos" || !td.References[0].Blocks() || !td.Deletable() {
		t.Fatalf("referencias: %+v", td.References)
	}
	if n, err := DeleteRows(ctx, c, "t-explora", "gente", []string{`["2"]`}); err != nil || n != 1 {
		t.Fatalf("eliminar: %d %v", n, err)
	}
	// El 1 tiene un hijo: falla y tampoco se elimina el 3 (todo o nada).
	if _, err := DeleteRows(ctx, c, "t-explora", "gente", []string{`["3"]`, `["1"]`}); err == nil || !strings.Contains(err.Error(), "hijos.gente_id") {
		t.Fatalf("debió fallar por la clave foránea: %v", err)
	}
	if n := count("SELECT count(*) FROM `t-explora`.`gente`"); n != 2 {
		t.Fatalf("quedaron %d", n)
	}
	if n, err := DeleteRows(ctx, c, "t-explora", "gente", []string{`["2"]`}); err != nil || n != 0 {
		t.Fatalf("ya eliminado: %d %v", n, err)
	}
	_, rows, _, _ = Rows(ctx, c, "t-explora", "bin", RowQuery{Limit: 50, Order: "x"})
	if len(rows) != 2 || rows[0].Key != `["00FF"]` || rows[0].Label != "u = 0x00FF" {
		t.Fatalf("clave binaria: %+v", rows)
	}
	if n, err := DeleteRows(ctx, c, "t-explora", "bin", []string{rows[0].Key}); err != nil || n != 1 || count("SELECT x FROM `t-explora`.`bin`") != 2 {
		t.Fatalf("eliminar binaria: %d %v", n, err)
	}
	for _, bad := range []struct{ table, key string }{{"sinclave", `["1"]`}, {"v_gente", `["1"]`}, {"gente", `1`}, {"gente", `["1","2"]`}} {
		if _, err := DeleteRows(ctx, c, "t-explora", bad.table, []string{bad.key}); err == nil {
			t.Errorf("%s %s debió fallar", bad.table, bad.key)
		}
	}
	if _, err := DeleteRows(ctx, c, "mysql", "user", []string{`["x"]`}); err == nil {
		t.Error("base del sistema")
	}

	// Crear bases vacías desde el explorador.
	db.ExecContext(ctx, "DROP DATABASE IF EXISTS `t_nueva`")
	defer db.ExecContext(ctx, "DROP DATABASE IF EXISTS `t_nueva`")
	if err := CreateEmptyDatabase(ctx, c, "t_nueva", "utf8mb4_unicode_ci"); err != nil {
		t.Fatal(err)
	}
	var coll string
	db.QueryRowContext(ctx, "SELECT default_collation_name FROM information_schema.schemata WHERE schema_name = 't_nueva'").Scan(&coll)
	if coll != "utf8mb4_unicode_ci" {
		t.Fatalf("intercalación: %q", coll)
	}
	if err := CreateEmptyDatabase(ctx, c, "t_nueva", "utf8mb4_unicode_ci"); err == nil || !strings.Contains(err.Error(), "ya existe") {
		t.Fatalf("duplicada: %v", err)
	}
	for _, bad := range [][2]string{{"t-guion", "utf8mb4_unicode_ci"}, {"mysql", "utf8mb4_unicode_ci"}, {"t_x", "latin1_swedish_ci"}, {"t_x`; DROP", "utf8mb4_bin"}} {
		if err := CreateEmptyDatabase(ctx, c, bad[0], bad[1]); err == nil {
			t.Errorf("%v debió fallar", bad)
		}
	}

	// La sesión es de solo lectura.
	conn, done, err := session(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	_, err = conn.ExecContext(ctx, "INSERT INTO `t-explora`.`gente` (nombre) VALUES ('x')")
	done()
	if err == nil || !strings.Contains(err.Error(), "READ ONLY") {
		t.Fatalf("debió rechazar la escritura: %v", err)
	}
}
