package web

import (
	"context"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"world/internal/dbmigrate"
)

// Pestaña SQL de una instancia: ejecutar scripts. Lo que solo lee se ejecuta directo; si el script
// cambia algo, primero se muestra la revisión (qué sentencias y cuáles son peligrosas) y se pide
// confirmación con Sweet Alert.

const scriptTimeout = 15 * time.Minute

func (s *Server) dbSQLData(ctx context.Context, v *dbInstanceView, c dbmigrate.Conn, db, script string) data {
	d := data{"Inst": v, "Tab": "sql", "SelDB": db, "Script": script, "Tx": true, "RowsLimit": dbmigrate.ScriptRowsLimit}
	if sum, err := dbmigrate.Summary(ctx, c); err == nil {
		var dbs []string
		for _, x := range sum.Databases {
			dbs = append(dbs, x.Name)
		}
		d["DBs"] = dbs
	}
	return d
}

func (s *Server) dbSQL(w http.ResponseWriter, r *http.Request) {
	v, c, ok := s.instance(r.PathValue("inst"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	s.render(w, r, "db_sql", s.dbSQLData(ctx, v, c, r.URL.Query().Get("db"), ""))
}

func (s *Server) dbSQLRun(w http.ResponseWriter, r *http.Request) {
	v, c, ok := s.instance(r.PathValue("inst"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	db := r.FormValue("db")
	script := r.FormValue("sql")
	if f, h, err := r.FormFile("file"); err == nil {
		defer f.Close()
		if h.Size > dbmigrate.MaxScriptBytes {
			script = ""
		} else if b, err := io.ReadAll(io.LimitReader(f, dbmigrate.MaxScriptBytes+1)); err == nil && len(b) > 0 {
			script = string(b)
		}
	}
	tx := r.FormValue("tx") == "1"

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	d := s.dbSQLData(ctx, v, c, db, script)
	cancel()
	d["Tx"] = tx
	if db != "" && !dbmigrate.ValidDBName(db) {
		d["Error"] = "Base inválida."
		s.render(w, r, "db_sql", d)
		return
	}
	stmts, err := dbmigrate.SplitScript(script)
	switch {
	case err != nil:
		d["Error"] = err.Error()
	case len(stmts) == 0:
		d["Error"] = "El script no tiene sentencias."
	}
	if d["Error"] != nil {
		s.render(w, r, "db_sql", d)
		return
	}
	plan := dbmigrate.Plan(stmts)
	d["Plan"] = plan
	// Si cambia algo, se ejecuta solo después de confirmar la revisión.
	if plan.Writes > 0 && r.FormValue("confirmed") != "1" {
		d["Review"] = true
		s.render(w, r, "db_sql", d)
		return
	}

	ctx, cancel = context.WithTimeout(r.Context(), scriptTimeout)
	defer cancel()
	run, err := dbmigrate.RunScript(ctx, c, stmts, dbmigrate.ScriptOptions{DB: db, Transaction: tx})
	status := "ok"
	switch {
	case err != nil:
		d["Error"] = err.Error()
		status = "no se conectó: " + err.Error()
	case run.Failed():
		status = "falló en la sentencia " + strconv.Itoa(len(run.Results))
	}
	log.Printf("bases: %s ejecutó un script en %s/%s (IP %s): %d sentencias, %s; transacción=%v; %s",
		currentUser(r).Email, v.Name, db, clientIP(r), len(stmts), orNone(plan.Summary()), tx, status)
	d["Run"] = run
	s.render(w, r, "db_sql", d)
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "solo lectura"
	}
	return s
}
