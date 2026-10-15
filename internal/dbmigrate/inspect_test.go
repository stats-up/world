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
	if r := rows[0]; r[1].Text != "José" || !r[2].Cut || len(r[2].Text) != cellMax || !r[3].Binary || r[3].Size != 4 {
		t.Fatalf("fila 1: %+v", r)
	}
	if r := rows[1]; !r[2].Null || !r[3].Null {
		t.Fatalf("fila 2: %+v", r)
	}
	// El filtro escapa % y _ (busca el texto literal), y una columna desconocida se ignora.
	_, rows, _, err = Rows(ctx, c, "t-explora", "gente", RowQuery{Limit: 50, FilterCol: "nombre", FilterVal: "%_"})
	if err != nil || len(rows) != 1 || rows[0][1].Text != "50%_off" {
		t.Fatalf("filtro: %v %v", rows, err)
	}
	_, rows, _, err = Rows(ctx, c, "t-explora", "gente", RowQuery{Limit: 50, Order: "id`; DROP TABLE gente; --", Desc: true, FilterCol: "x", FilterVal: "y"})
	if err != nil || len(rows) != 3 {
		t.Fatalf("columnas inválidas: %v %v", rows, err)
	}
	_, rows, _, err = Rows(ctx, c, "t-explora", "gente", RowQuery{Limit: 1, Offset: 2, Order: "nombre", Desc: true})
	if err != nil || len(rows) != 1 || rows[0][1].Text != "50%_off" { // José, Ana, 50%_off
		t.Fatalf("orden/página: %v %v", rows, err)
	}
	if _, _, _, err := Rows(ctx, c, "t-explora", "no-existe", RowQuery{}); err != ErrNotFound {
		t.Fatal(err)
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
