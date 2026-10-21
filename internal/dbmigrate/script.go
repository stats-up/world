package dbmigrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Scripts SQL: se separan en sentencias (como el cliente mysql: comillas, comentarios y DELIMITER),
// se clasifican para pedir confirmación antes de cambiar datos y se ejecutan en una sola conexión.

const (
	MaxScriptBytes  = 4 << 20
	MaxStatements   = 2000
	ScriptRowsLimit = 500 // filas que se muestran por resultado
)

// Statement es una sentencia del script.
type Statement struct {
	SQL  string
	Line int    // línea donde empieza (desde 1)
	Kind string // primera palabra en mayúsculas (SELECT, UPDATE, CREATE TABLE…)
}

// Read: no cambia datos ni estructura (consultas, SHOW, EXPLAIN, USE, SET de la sesión).
func (s Statement) Read() bool {
	switch firstWord(s.Kind) {
	case "SELECT", "SHOW", "DESCRIBE", "DESC", "EXPLAIN", "WITH", "TABLE", "VALUES", "HELP", "USE", "SET", "CHECKSUM":
		return !(firstWord(s.Kind) == "SELECT" && selectInto.MatchString(s.SQL))
	}
	return false
}

// Danger: borra o reemplaza en bloque (DROP, TRUNCATE, DELETE/UPDATE sin WHERE).
func (s Statement) Danger() string {
	w := firstWord(s.Kind)
	switch {
	case w == "DROP" || w == "TRUNCATE":
		return s.Kind
	case (w == "DELETE" || w == "UPDATE") && !hasWhere.MatchString(stripStrings(s.SQL)):
		return w + " sin WHERE (afecta todas las filas)"
	}
	return ""
}

// Short: el comienzo de la sentencia en una línea, para listarla.
func (s Statement) Short() string {
	t := strings.Join(strings.Fields(s.SQL), " ")
	if r := []rune(t); len(r) > 160 {
		return string(r[:160]) + "…"
	}
	return t
}

var (
	selectInto = regexp.MustCompile(`(?is)\bINTO\s+(OUTFILE|DUMPFILE)\b`)
	hasWhere   = regexp.MustCompile(`(?i)\bWHERE\b`)
	// Segunda palabra que vale la pena mostrar (CREATE TABLE, DROP DATABASE, ALTER USER…).
	kindObject = regexp.MustCompile(`(?i)^(CREATE|DROP|ALTER|RENAME|TRUNCATE)\s+(OR\s+REPLACE\s+)?(TEMPORARY\s+|UNIQUE\s+|FULLTEXT\s+|SPATIAL\s+|DEFINER\s*=\s*\S+\s+|ALGORITHM\s*=\s*\S+\s+|SQL\s+SECURITY\s+\S+\s+)*(TABLE|DATABASE|SCHEMA|INDEX|VIEW|PROCEDURE|FUNCTION|TRIGGER|EVENT|USER|ROLE)\b`)
)

func firstWord(kind string) string {
	w, _, _ := strings.Cut(kind, " ")
	return w
}

// stripStrings quita el contenido de los textos entre comillas (para buscar WHERE sin falsos positivos).
func stripStrings(s string) string {
	var b strings.Builder
	var q byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q != 0 && c == '\\' && q != '`':
			i++
		case q != 0 && c == q:
			q = 0
			b.WriteByte(c)
		case q != 0:
		case c == '\'' || c == '"' || c == '`':
			q = c
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func statementKind(sql string) string {
	t := strings.TrimLeft(sql, " \t\r\n(")
	if strings.HasPrefix(t, "/*!") { // código condicional de mysqldump: /*!40101 SET NAMES … */
		t = strings.TrimLeft(t[3:], "0123456789 \t\r\n(")
	}
	if m := kindObject.FindStringSubmatch(t); m != nil {
		return strings.ToUpper(m[1] + " " + m[4])
	}
	end := strings.IndexFunc(t, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_') })
	if end < 0 {
		end = len(t)
	}
	return strings.ToUpper(t[:end])
}

// SplitScript separa el script en sentencias. Respeta textos entre comillas, identificadores con
// backticks, comentarios (-- , #, /* */) y líneas DELIMITER (para procedimientos y triggers).
func SplitScript(script string) ([]Statement, error) {
	if len(script) > MaxScriptBytes {
		return nil, fmt.Errorf("el script supera %d MB", MaxScriptBytes>>20)
	}
	var out []Statement
	delim := ";"
	var cur strings.Builder
	line, startLine := 1, 0
	atLineStart := true
	flush := func() {
		s := strings.TrimSpace(cur.String())
		cur.Reset()
		if s != "" && !onlyComments(s) {
			out = append(out, Statement{SQL: s, Line: startLine, Kind: statementKind(stripLeadingComments(s))})
		}
		startLine = 0
	}
	n := len(script)
	for i := 0; i < n; {
		c := script[i]
		// DELIMITER al comienzo de una línea (solo fuera de una sentencia a medias o al inicio).
		if atLineStart {
			j := i
			for j < n && (script[j] == ' ' || script[j] == '\t') {
				j++
			}
			if j+9 < n && strings.EqualFold(script[j:j+9], "DELIMITER") && (script[j+9] == ' ' || script[j+9] == '\t') {
				end := strings.IndexByte(script[j:], '\n')
				if end < 0 {
					end = n - j
				}
				d := strings.TrimSpace(script[j+9 : j+end])
				if d == "" {
					return nil, fmt.Errorf("línea %d: DELIMITER sin valor", line)
				}
				if strings.TrimSpace(cur.String()) != "" {
					flush()
				}
				delim = d
				i = j + end
				continue
			}
		}
		atLineStart = false
		switch {
		case c == '\n':
			line++
			atLineStart = true
			cur.WriteByte(c)
			i++
			continue
		case c == '\'' || c == '"' || c == '`':
			if startLine == 0 {
				startLine = line
			}
			j := i + 1
			for ; j < n; j++ {
				if script[j] == '\n' {
					line++
				}
				if script[j] == '\\' && c != '`' {
					j++
					continue
				}
				if script[j] == c {
					if j+1 < n && script[j+1] == c { // comilla duplicada = escapada
						j++
						continue
					}
					break
				}
			}
			if j >= n {
				return nil, fmt.Errorf("línea %d: falta cerrar %c", startLine, c)
			}
			cur.WriteString(script[i : j+1])
			i = j + 1
			continue
		case c == '#' || (c == '-' && i+2 < n && script[i+1] == '-' && (script[i+2] == ' ' || script[i+2] == '\t' || script[i+2] == '\n' || script[i+2] == '\r')) ||
			(c == '-' && i+2 == n && script[i+1] == '-'):
			end := strings.IndexByte(script[i:], '\n')
			if end < 0 {
				end = n - i
			}
			cur.WriteString(script[i : i+end])
			i += end
			continue
		case c == '/' && i+1 < n && script[i+1] == '*':
			end := strings.Index(script[i+2:], "*/")
			if end < 0 {
				return nil, fmt.Errorf("línea %d: comentario /* sin cerrar", line)
			}
			if startLine == 0 && i+2 < n && script[i+2] == '!' {
				startLine = line // /*!50001 … */: código condicional, es parte de la sentencia
			}
			body := script[i : i+2+end+2]
			line += strings.Count(body, "\n")
			cur.WriteString(body)
			i += len(body)
			continue
		case strings.HasPrefix(script[i:], delim):
			flush()
			i += len(delim)
			if len(out) > MaxStatements {
				return nil, fmt.Errorf("el script tiene más de %d sentencias", MaxStatements)
			}
			continue
		}
		if startLine == 0 && c != ' ' && c != '\t' && c != '\r' {
			startLine = line
		}
		cur.WriteByte(c)
		i++
	}
	flush()
	if len(out) > MaxStatements {
		return nil, fmt.Errorf("el script tiene más de %d sentencias", MaxStatements)
	}
	return out, nil
}

func stripLeadingComments(s string) string {
	for {
		s = strings.TrimSpace(s)
		switch {
		case strings.HasPrefix(s, "--") || strings.HasPrefix(s, "#"):
			_, rest, ok := strings.Cut(s, "\n")
			if !ok {
				return ""
			}
			s = rest
		case strings.HasPrefix(s, "/*") && !strings.HasPrefix(s, "/*!"):
			_, rest, ok := strings.Cut(s, "*/")
			if !ok {
				return ""
			}
			s = rest
		default:
			return s
		}
	}
}

func onlyComments(s string) bool { return stripLeadingComments(s) == "" }

// ScriptPlan resume qué hace el script antes de ejecutarlo.
type ScriptPlan struct {
	Statements []Statement
	Writes     int
	Kinds      []KindCount // sentencias que cambian algo, por tipo
	Dangers    []string
}

type KindCount struct {
	Kind  string
	Count int
}

func Plan(stmts []Statement) ScriptPlan {
	p := ScriptPlan{Statements: stmts}
	idx := map[string]int{}
	for _, s := range stmts {
		if d := s.Danger(); d != "" {
			p.Dangers = append(p.Dangers, fmt.Sprintf("línea %d: %s", s.Line, d))
		}
		if s.Read() {
			continue
		}
		p.Writes++
		if i, ok := idx[s.Kind]; ok {
			p.Kinds[i].Count++
		} else {
			idx[s.Kind] = len(p.Kinds)
			p.Kinds = append(p.Kinds, KindCount{s.Kind, 1})
		}
	}
	return p
}

// Summary: "2 UPDATE, 1 DROP TABLE".
func (p ScriptPlan) Summary() string {
	parts := make([]string, len(p.Kinds))
	for i, k := range p.Kinds {
		parts[i] = fmt.Sprintf("%d %s", k.Count, k.Kind)
	}
	return strings.Join(parts, ", ")
}

// StatementResult es lo que devolvió una sentencia.
type StatementResult struct {
	Statement
	Columns  []string
	Rows     [][]Cell
	Total    int64 // filas devueltas (pueden ser más que las mostradas)
	Sets     int   // conjuntos de resultados (CALL puede devolver varios)
	Affected int64
	InsertID int64
	HasRows  bool
	Duration time.Duration
	Warnings int
	Err      string
}

func (r StatementResult) More() bool { return r.Total > int64(len(r.Rows)) }

type ScriptOptions struct {
	DB          string // base por defecto (USE); vacío = ninguna
	Transaction bool   // todo o nada (salvo DDL, que MySQL confirma solo)
}

// ScriptRun es el resultado completo.
type ScriptRun struct {
	Results    []StatementResult
	Skipped    int  // sentencias que no se ejecutaron por un error anterior
	RolledBack bool // se deshizo la transacción por un error
	Committed  bool
	Duration   time.Duration
}

func (r *ScriptRun) Failed() bool {
	return len(r.Results) > 0 && r.Results[len(r.Results)-1].Err != ""
}

// returnsRows: sentencias que se leen con Query (el resto con Exec, que informa filas afectadas).
func returnsRows(kind string) bool {
	switch firstWord(kind) {
	case "SELECT", "SHOW", "DESCRIBE", "DESC", "EXPLAIN", "WITH", "TABLE", "VALUES", "HELP", "CALL",
		"CHECK", "ANALYZE", "OPTIMIZE", "REPAIR", "CHECKSUM":
		return true
	}
	return false
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// RunScript ejecuta las sentencias en orden en una sola conexión y se detiene en el primer error.
func RunScript(ctx context.Context, c Conn, stmts []Statement, opt ScriptOptions) (*ScriptRun, error) {
	c.Database = opt.DB
	pool, err := openTimeout(ctx, c, 30*time.Minute)
	if err != nil {
		return nil, err
	}
	defer pool.Close()
	conn, err := pool.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	run := &ScriptRun{}
	start := time.Now()
	defer func() { run.Duration = time.Since(start) }()
	var ex execer = conn
	var tx *sql.Tx
	if opt.Transaction {
		if tx, err = conn.BeginTx(ctx, nil); err != nil {
			return nil, err
		}
		ex = tx
	}
	for i, s := range stmts {
		res := runStatement(ctx, ex, s)
		run.Results = append(run.Results, res)
		if res.Err != "" {
			run.Skipped = len(stmts) - i - 1
			if tx != nil {
				tx.Rollback()
				run.RolledBack = true
			}
			return run, nil
		}
	}
	if tx != nil {
		if err := tx.Commit(); err != nil {
			run.RolledBack = true
			run.Results = append(run.Results, StatementResult{Statement: Statement{SQL: "COMMIT", Kind: "COMMIT"}, Err: err.Error()})
			return run, nil
		}
		run.Committed = true
	}
	return run, nil
}

func runStatement(ctx context.Context, ex execer, s Statement) StatementResult {
	r := StatementResult{Statement: s}
	t0 := time.Now()
	defer func() { r.Duration = time.Since(t0) }()
	if returnsRows(s.Kind) {
		rows, err := ex.QueryContext(ctx, s.SQL)
		if err != nil {
			r.Err = scriptError(err)
			return r
		}
		defer rows.Close()
		r.HasRows = true
		for {
			r.Sets++
			if err := readResultSet(rows, &r); err != nil {
				r.Err = scriptError(err)
				return r
			}
			if !rows.NextResultSet() {
				break
			}
		}
		if err := rows.Err(); err != nil {
			r.Err = scriptError(err)
		}
	} else {
		res, err := ex.ExecContext(ctx, s.SQL)
		if err != nil {
			r.Err = scriptError(err)
			return r
		}
		r.Affected, _ = res.RowsAffected()
		r.InsertID, _ = res.LastInsertId()
	}
	if r.Err == "" {
		if row, err := ex.QueryContext(ctx, "SELECT @@warning_count"); err == nil {
			if row.Next() {
				row.Scan(&r.Warnings)
			}
			row.Close()
		}
	}
	return r
}

// readResultSet guarda las primeras filas del primer conjunto con columnas y cuenta el resto.
func readResultSet(rows *sql.Rows, r *StatementResult) error {
	cols, err := rows.ColumnTypes()
	if err != nil {
		return err
	}
	first := r.Columns == nil && len(cols) > 0
	if first {
		for _, ct := range cols {
			r.Columns = append(r.Columns, ct.Name())
		}
	}
	raw := make([]sql.RawBytes, len(cols))
	ptrs := make([]any, len(cols))
	for i := range raw {
		ptrs[i] = &raw[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		if !first {
			continue
		}
		r.Total++
		if len(r.Rows) >= ScriptRowsLimit {
			continue
		}
		row := make([]Cell, len(cols))
		for i, v := range raw {
			switch {
			case v == nil:
				row[i] = Cell{Null: true}
			case binaryType(cols[i].DatabaseTypeName()):
				row[i] = Cell{Binary: true, Size: int64(len(v))}
			default:
				row[i] = textCell(string(v))
			}
		}
		r.Rows = append(r.Rows, row)
	}
	return nil
}

func binaryType(t string) bool {
	switch strings.ToUpper(t) {
	case "BLOB", "TINYBLOB", "MEDIUMBLOB", "LONGBLOB", "BINARY", "VARBINARY", "GEOMETRY", "BIT":
		return true
	}
	return false
}

// scriptError deja el mensaje del motor (ya trae el código y la línea) sin el ruido del driver.
func scriptError(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "se canceló o superó el tiempo máximo: " + err.Error()
	}
	return err.Error()
}

func (r StatementResult) Took() string { return took(r.Duration) }
func (r *ScriptRun) Took() string      { return took(r.Duration) }

func took(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%d ms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1f s", d.Seconds())
	}
	return fmt.Sprintf("%d min %d s", int(d.Minutes()), int(d.Seconds())%60)
}

// FailedLine: línea de la sentencia que falló (0 si no falló ninguna).
func (r *ScriptRun) FailedLine() int {
	if !r.Failed() {
		return 0
	}
	return r.Results[len(r.Results)-1].Line
}
