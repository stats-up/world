package dbmigrate

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Explorador de solo lectura: bases, tablas, estructura y datos de una instancia MySQL/MariaDB.
// Cada consulta corre en una sesión READ ONLY y con tiempo máximo, para no cargar el servidor.

// ServerSummary es lo que se muestra de una instancia.
type ServerSummary struct {
	Version   string
	Databases []DBSummary
}

type DBSummary struct {
	Name      string
	Charset   string
	Collation string
	Tables    int
	Views     int
	Rows      int64 // aproximado
	Bytes     int64 // datos + índices
	System    bool
}

type TableSummary struct {
	Name       string
	Type       string // BASE TABLE | VIEW
	Engine     string
	Collation  string
	Rows       int64 // aproximado
	DataBytes  int64
	IndexBytes int64
	Updated    time.Time
	Comment    string
}

func (t *TableSummary) View() bool   { return t.Type == "VIEW" }
func (t *TableSummary) Bytes() int64 { return t.DataBytes + t.IndexBytes }

type Column struct {
	Name      string
	Type      string // ej: varchar(255)
	DataType  string // ej: varchar
	Nullable  bool
	Key       string // PRI, UNI, MUL
	Default   sql.NullString
	Extra     string
	Collation string
	Comment   string
}

// Binary: el valor no es texto (se muestra solo su tamaño).
func (c *Column) Binary() bool {
	switch c.DataType {
	case "binary", "varbinary", "tinyblob", "blob", "mediumblob", "longblob", "geometry", "point", "linestring",
		"polygon", "multipoint", "multilinestring", "multipolygon", "geometrycollection", "bit":
		return true
	}
	return false
}

// Long: texto que puede ser muy grande (se trae recortado).
func (c *Column) Long() bool {
	switch c.DataType {
	case "tinytext", "text", "mediumtext", "longtext", "json":
		return true
	}
	return false
}

type Index struct {
	Name    string
	Unique  bool
	Type    string
	Columns string
}

// Cell es un valor de la grilla de datos.
type Cell struct {
	Text   string
	Null   bool
	Binary bool  // solo se muestra el tamaño
	Size   int64 // bytes (binarios y textos recortados)
	Cut    bool  // texto recortado
}

// RowQuery pide una página de datos.
type RowQuery struct {
	Offset    int
	Limit     int
	Order     string // columna (validada contra la tabla)
	Desc      bool
	FilterCol string
	FilterVal string // contiene (LIKE)
}

const (
	cellMax     = 300
	queryTimeMS = 20000
)

// session abre una conexión de solo lectura con tiempo máximo por consulta.
func session(ctx context.Context, c Conn) (*sql.Conn, func(), error) {
	c.Database = ""
	pool, err := open(ctx, c)
	if err != nil {
		return nil, nil, err
	}
	conn, err := pool.Conn(ctx)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	done := func() { conn.Close(); pool.Close() }
	if _, err := conn.ExecContext(ctx, `SET SESSION TRANSACTION READ ONLY`); err != nil {
		done()
		return nil, nil, err
	}
	// MySQL (milisegundos) y MariaDB (segundos); cada uno ignora la del otro.
	conn.ExecContext(ctx, fmt.Sprintf(`SET SESSION max_execution_time = %d`, queryTimeMS))
	conn.ExecContext(ctx, fmt.Sprintf(`SET SESSION max_statement_time = %d`, queryTimeMS/1000))
	conn.ExecContext(ctx, `SET SESSION information_schema_stats_expiry = 0`)
	return conn, done, nil
}

// Summary: versión y bases de la instancia, con tamaño y cantidad de tablas.
func Summary(ctx context.Context, c Conn) (*ServerSummary, error) {
	conn, done, err := session(ctx, c)
	if err != nil {
		return nil, err
	}
	defer done()
	s := &ServerSummary{}
	if err := conn.QueryRowContext(ctx, `SELECT VERSION()`).Scan(&s.Version); err != nil {
		return nil, err
	}
	rows, err := conn.QueryContext(ctx, `SELECT s.schema_name, s.default_character_set_name, s.default_collation_name,
		COALESCE(SUM(t.table_type = 'BASE TABLE'), 0), COALESCE(SUM(t.table_type = 'VIEW'), 0),
		COALESCE(SUM(t.table_rows), 0), COALESCE(SUM(t.data_length + t.index_length), 0)
		FROM information_schema.schemata s LEFT JOIN information_schema.tables t ON t.table_schema = s.schema_name
		GROUP BY s.schema_name, s.default_character_set_name, s.default_collation_name ORDER BY s.schema_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var d DBSummary
		if err := rows.Scan(&d.Name, &d.Charset, &d.Collation, &d.Tables, &d.Views, &d.Rows, &d.Bytes); err != nil {
			return nil, err
		}
		d.System = systemDB(d.Name)
		s.Databases = append(s.Databases, d)
	}
	return s, rows.Err()
}

// Tables lista las tablas y vistas de una base (ErrNoDatabase si no existe).
func Tables(ctx context.Context, c Conn, db string) ([]TableSummary, error) {
	conn, done, err := session(ctx, c)
	if err != nil {
		return nil, err
	}
	defer done()
	var n int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.schemata WHERE schema_name = ?`, db).Scan(&n); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, ErrNotFound
	}
	rows, err := conn.QueryContext(ctx, `SELECT table_name, table_type, COALESCE(engine, ''), COALESCE(table_collation, ''),
		COALESCE(table_rows, 0), COALESCE(data_length, 0), COALESCE(index_length, 0), COALESCE(UNIX_TIMESTAMP(update_time), 0),
		COALESCE(table_comment, '') FROM information_schema.tables WHERE table_schema = ? ORDER BY table_name`, db)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TableSummary
	for rows.Next() {
		var t TableSummary
		var updated int64
		if err := rows.Scan(&t.Name, &t.Type, &t.Engine, &t.Collation, &t.Rows, &t.DataBytes, &t.IndexBytes, &updated, &t.Comment); err != nil {
			return nil, err
		}
		if updated > 0 {
			t.Updated = time.Unix(updated, 0)
		}
		if t.Type == "SYSTEM VIEW" {
			t.Type = "VIEW"
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

var ErrNotFound = fmt.Errorf("no existe")

// TableDetail es la estructura de una tabla o vista.
type TableDetail struct {
	TableSummary
	Columns []Column
	Indexes []Index
	Create  string
}

func (t *TableDetail) Column(name string) *Column {
	for i := range t.Columns {
		if t.Columns[i].Name == name {
			return &t.Columns[i]
		}
	}
	return nil
}

// Table devuelve la estructura de una tabla (ErrNotFound si no existe).
func Table(ctx context.Context, c Conn, db, table string) (*TableDetail, error) {
	conn, done, err := session(ctx, c)
	if err != nil {
		return nil, err
	}
	defer done()
	return tableDetail(ctx, conn, db, table)
}

func tableDetail(ctx context.Context, conn *sql.Conn, db, table string) (*TableDetail, error) {
	t := &TableDetail{}
	var updated int64
	err := conn.QueryRowContext(ctx, `SELECT table_name, table_type, COALESCE(engine, ''), COALESCE(table_collation, ''),
		COALESCE(table_rows, 0), COALESCE(data_length, 0), COALESCE(index_length, 0), COALESCE(UNIX_TIMESTAMP(update_time), 0),
		COALESCE(table_comment, '') FROM information_schema.tables WHERE table_schema = ? AND table_name = ?`, db, table).
		Scan(&t.Name, &t.Type, &t.Engine, &t.Collation, &t.Rows, &t.DataBytes, &t.IndexBytes, &updated, &t.Comment)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if updated > 0 {
		t.Updated = time.Unix(updated, 0)
	}
	if t.Type == "SYSTEM VIEW" {
		t.Type = "VIEW"
	}
	rows, err := conn.QueryContext(ctx, `SELECT column_name, column_type, data_type, is_nullable = 'YES', column_key, column_default,
		extra, COALESCE(collation_name, ''), column_comment FROM information_schema.columns
		WHERE table_schema = ? AND table_name = ? ORDER BY ordinal_position`, db, table)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var col Column
		if err := rows.Scan(&col.Name, &col.Type, &col.DataType, &col.Nullable, &col.Key, &col.Default, &col.Extra, &col.Collation, &col.Comment); err != nil {
			rows.Close()
			return nil, err
		}
		col.DataType = strings.ToLower(col.DataType)
		t.Columns = append(t.Columns, col)
	}
	rows.Close()
	rows, err = conn.QueryContext(ctx, `SELECT index_name, MIN(non_unique) = 0, MIN(index_type),
		GROUP_CONCAT(column_name ORDER BY seq_in_index SEPARATOR ', ')
		FROM information_schema.statistics WHERE table_schema = ? AND table_name = ?
		GROUP BY index_name ORDER BY index_name <> 'PRIMARY', index_name`, db, table)
	if err == nil {
		for rows.Next() {
			var ix Index
			var cols sql.NullString
			if rows.Scan(&ix.Name, &ix.Unique, &ix.Type, &cols) == nil {
				ix.Columns = cols.String
				t.Indexes = append(t.Indexes, ix)
			}
		}
		rows.Close()
	}
	// SHOW CREATE devuelve 2 columnas en tablas y 4 en vistas: la definición es la segunda.
	if r, err := conn.QueryContext(ctx, `SHOW CREATE TABLE `+quoteIdent(db)+`.`+quoteIdent(table)); err == nil {
		if cols, _ := r.Columns(); r.Next() && len(cols) >= 2 {
			vals := make([]sql.NullString, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if r.Scan(ptrs...) == nil {
				t.Create = vals[1].String
			}
		}
		r.Close()
	}
	return t, nil
}

// Rows trae una página de datos. Los textos largos se recortan y los binarios se reemplazan por su tamaño.
func Rows(ctx context.Context, c Conn, db, table string, q RowQuery) (*TableDetail, [][]Cell, bool, error) {
	conn, done, err := session(ctx, c)
	if err != nil {
		return nil, nil, false, err
	}
	defer done()
	t, err := tableDetail(ctx, conn, db, table)
	if err != nil {
		return nil, nil, false, err
	}
	q.Limit = min(max(q.Limit, 1), 200)
	q.Offset = max(q.Offset, 0)
	sel := make([]string, len(t.Columns))
	for i, col := range t.Columns {
		id := quoteIdent(col.Name)
		switch {
		case col.Binary():
			sel[i] = "OCTET_LENGTH(" + id + ")"
		case col.Long():
			sel[i] = fmt.Sprintf("LEFT(%s, %d)", id, cellMax+1)
		default:
			sel[i] = id
		}
	}
	query := "SELECT " + strings.Join(sel, ", ") + " FROM " + quoteIdent(db) + "." + quoteIdent(table)
	var args []any
	if col := t.Column(q.FilterCol); col != nil && q.FilterVal != "" {
		query += " WHERE " + quoteIdent(col.Name) + " LIKE ?"
		args = append(args, "%"+escapeLike(q.FilterVal)+"%")
	}
	if col := t.Column(q.Order); col != nil {
		query += " ORDER BY " + quoteIdent(col.Name)
		if q.Desc {
			query += " DESC"
		}
	}
	query += fmt.Sprintf(" LIMIT %d, %d", q.Offset, q.Limit+1) // una de más: saber si hay otra página
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return t, nil, false, err
	}
	defer rows.Close()
	var out [][]Cell
	raw := make([]sql.RawBytes, len(t.Columns))
	ptrs := make([]any, len(raw))
	for i := range raw {
		ptrs[i] = &raw[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return t, nil, false, err
		}
		row := make([]Cell, len(raw))
		for i, v := range raw {
			col := &t.Columns[i]
			switch {
			case v == nil:
				row[i] = Cell{Null: true}
			case col.Binary():
				var n int64
				fmt.Sscan(string(v), &n)
				row[i] = Cell{Binary: true, Size: n}
			default:
				row[i] = textCell(string(v))
			}
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return t, nil, false, err
	}
	more := len(out) > q.Limit
	if more {
		out = out[:q.Limit]
	}
	return t, out, more, nil
}

func textCell(s string) Cell {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	if utf8.RuneCountInString(s) <= cellMax {
		return Cell{Text: s}
	}
	r := []rune(s)
	return Cell{Text: string(r[:cellMax]), Cut: true}
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
