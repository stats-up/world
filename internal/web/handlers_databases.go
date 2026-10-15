package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"world/internal/dbmigrate"
	"world/internal/store"
)

// Bases de datos: migrar una base MySQL/MariaDB a MainDB. La contraseña de origen se pide
// cada vez (para probar y para migrar) y no se guarda.

// ResumeDBMigrations retoma el seguimiento de las migraciones que seguían corriendo.
func (s *Server) ResumeDBMigrations(ctx context.Context) { s.dbmig.Resume(ctx) }

func (s *Server) mainDB() dbmigrate.Conn { return dbmigrate.MainDB(s.st, s.box) }

func (s *Server) databasesIndex(w http.ResponseWriter, r *http.Request) {
	s.renderDatabases(w, r, map[string]string{"src_port": "3306"}, "")
}

func (s *Server) renderDatabases(w http.ResponseWriter, r *http.Request, form map[string]string, errMsg string) {
	migs, err := s.st.DBMigrations(30)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	main := s.mainDB()
	main.Password = "" // nunca vuelve al navegador
	s.render(w, r, "databases", data{"MainDB": main, "MainReady": dbmigrate.Configured(s.mainDB()),
		"Migrations": migs, "Form": form, "Error": errMsg})
}

func (s *Server) mainDBSubmit(w http.ResponseWriter, r *http.Request) {
	port, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("port")))
	if port == 0 {
		port = 3306
	}
	c := dbmigrate.Conn{Host: strings.TrimSpace(r.FormValue("host")), Port: port,
		User: strings.TrimSpace(r.FormValue("user")), Password: r.FormValue("password")}
	back := func(kind, msg string) {
		s.setFlash(w, kind, msg)
		redirect(w, r, "/databases#maindb")
	}
	if !validDBHost(c.Host) || c.User == "" || port < 1 || port > 65535 {
		back("error", "Revisa el host, el puerto y el usuario de MainDB.")
		return
	}
	test := c
	if test.Password == "" {
		test.Password = s.mainDB().Password // vacío = conservar la contraseña guardada
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	t, err := dbmigrate.ProbeTarget(ctx, test, "world_probe")
	if err != nil {
		back("error", "No se guardó: "+err.Error())
		return
	}
	if err := dbmigrate.SaveMainDB(s.st, s.box, c); err != nil {
		back("error", "No se pudo guardar: "+err.Error())
		return
	}
	back("ok", "Conexión a MainDB guardada ("+t.Version+").")
}

func validDBHost(h string) bool { return validImportHost(strings.ToLower(h)) }

// migrationForm lee el formulario de migración (sin la contraseña) y arma la conexión de origen.
func migrationForm(r *http.Request) (map[string]string, dbmigrate.Conn, string) {
	form := map[string]string{}
	for _, k := range []string{"src_host", "src_port", "src_user", "src_db", "dst_db"} {
		form[k] = strings.TrimSpace(r.FormValue(k))
	}
	form["to_utf8"] = "0"
	if r.FormValue("to_utf8") == "1" {
		form["to_utf8"] = "1"
	}
	port, _ := strconv.Atoi(form["src_port"])
	src := dbmigrate.Conn{Host: form["src_host"], Port: port, User: form["src_user"], Password: r.FormValue("src_pass"), Database: form["src_db"]}
	switch {
	case !validDBHost(src.Host):
		return form, src, "Host de origen inválido."
	case port < 1 || port > 65535:
		return form, src, "Puerto de origen inválido."
	case src.User == "" || src.Password == "":
		return form, src, "Falta el usuario o la contraseña del origen."
	case !dbmigrate.ValidDBName(src.Database):
		return form, src, "Nombre de la base de origen inválido."
	case !dbmigrate.ValidNewName(form["dst_db"]):
		return form, src, "Nombre de la base nueva inválido: solo letras, números y guion bajo (máx. 64)."
	}
	return form, src, ""
}

type probeResult struct {
	Source    *dbmigrate.SourceInfo
	SourceErr string
	Target    *dbmigrate.TargetInfo
	TargetErr string
	DstDB     string
	Blocking  []string // impiden migrar
	Warnings  []string
}

func (p *probeResult) OK() bool { return len(p.Blocking) == 0 }

// probe prueba las dos conexiones y junta lo que impide migrar o conviene saber.
func (s *Server) probe(ctx context.Context, src dbmigrate.Conn, dst string) *probeResult {
	p := &probeResult{DstDB: dst}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if info, err := dbmigrate.ProbeSource(ctx, src); err != nil {
		p.SourceErr = err.Error()
		p.Blocking = append(p.Blocking, "No se pudo leer el origen.")
	} else {
		p.Source = info
		p.Warnings = append(p.Warnings, info.Warnings...)
	}
	main := s.mainDB()
	if !dbmigrate.Configured(main) {
		p.TargetErr = "Falta configurar la conexión a MainDB (más abajo)."
		p.Blocking = append(p.Blocking, p.TargetErr)
		return p
	}
	t, err := dbmigrate.ProbeTarget(ctx, main, dst)
	if err != nil {
		p.TargetErr = err.Error()
		p.Blocking = append(p.Blocking, "No se pudo conectar a MainDB.")
		return p
	}
	p.Target = t
	if t.Exists && t.Tables > 0 {
		p.Blocking = append(p.Blocking, "La base "+dst+" ya existe en MainDB y tiene "+strconv.Itoa(t.Tables)+" tabla(s). Elige otro nombre: world no sobrescribe bases con datos.")
	}
	if p.Source != nil && t.BinlogFix && (p.Source.Triggers > 0 || p.Source.Routines > 0) {
		p.Blocking = append(p.Blocking, "El origen tiene triggers o procedimientos y MainDB no permite crearlos todavía: en Lightsail ve a la base de datos → Parámetros y pon log_bin_trust_function_creators = 1.")
	}
	return p
}

func (s *Server) databaseProbe(w http.ResponseWriter, r *http.Request) {
	_, src, errMsg := migrationForm(r)
	if errMsg != "" {
		s.renderPartial(w, "db_probe", &probeResult{Blocking: []string{errMsg}})
		return
	}
	s.renderPartial(w, "db_probe", s.probe(r.Context(), src, r.FormValue("dst_db")))
}

func (s *Server) migrationCreate(w http.ResponseWriter, r *http.Request) {
	form, src, errMsg := migrationForm(r)
	if errMsg != "" {
		s.renderDatabases(w, r, form, errMsg)
		return
	}
	p := s.probe(r.Context(), src, form["dst_db"])
	if !p.OK() {
		msg := strings.Join(p.Blocking, " ")
		if p.SourceErr != "" {
			msg += " Origen: " + p.SourceErr
		}
		if p.TargetErr != "" && !strings.Contains(msg, p.TargetErr) {
			msg += " MainDB: " + p.TargetErr
		}
		s.renderDatabases(w, r, form, msg)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute) // puede descargar la imagen de MySQL
	defer cancel()
	req := dbmigrate.Request{Source: src, DstDB: form["dst_db"], Charset: p.Source.Charset,
		Collate: p.Source.Collation, Tables: p.Source.Tables, Bytes: p.Source.Bytes, ToUTF8: form["to_utf8"] == "1"}
	if req.ToUTF8 && strings.HasPrefix(strings.ToLower(req.Charset), "latin1") {
		req.Charset, req.Collate = "utf8mb4", "utf8mb4_unicode_ci"
	}
	id, err := s.dbmig.Start(ctx, req)
	if err != nil {
		s.renderDatabases(w, r, form, "No se pudo iniciar: "+err.Error())
		return
	}
	redirect(w, r, "/databases/migrations/"+strconv.FormatInt(id, 10))
}

func (s *Server) loadMigration(w http.ResponseWriter, r *http.Request) (*store.DBMigration, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return nil, false
	}
	m, err := s.st.DBMigration(id)
	if err != nil {
		http.NotFound(w, r)
		return nil, false
	}
	return m, true
}

func (s *Server) migrationData(m *store.DBMigration, csrf string) data {
	return data{"Mig": m, "Log": lastLines(s.dbmig.Log(m.ID), 400), "CSRF": csrf, "MainHost": s.mainDB().Host}
}

func (s *Server) migrationShow(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMigration(w, r)
	if !ok {
		return
	}
	s.render(w, r, "db_migration", data{"Mig": m, "Status": s.migrationData(m, sessionCSRF(r))})
}

func (s *Server) migrationStatus(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMigration(w, r)
	if !ok {
		return
	}
	if !m.Running() {
		w.Header().Set("HX-Refresh", "true") // terminó: recargar para mostrar el resultado completo
	}
	s.renderPartial(w, "db_migration_status", s.migrationData(m, sessionCSRF(r)))
}

func (s *Server) migrationCancel(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMigration(w, r)
	if !ok {
		return
	}
	back := "/databases/migrations/" + strconv.FormatInt(m.ID, 10)
	if !m.Running() {
		redirect(w, r, back)
		return
	}
	if err := s.dbmig.Cancel(r.Context(), m.ID); err != nil {
		s.mailBack(w, r, back, "error", "No se pudo cancelar: "+err.Error())
		return
	}
	s.mailBack(w, r, back, "ok", "Migración cancelada. La base "+m.DstDB+" quedó a medias en MainDB.")
}

// migrationDrop vacía (borra) la base de destino de una migración fallida o cancelada.
func (s *Server) migrationDrop(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMigration(w, r)
	if !ok {
		return
	}
	back := "/databases/migrations/" + strconv.FormatInt(m.ID, 10)
	if m.Status != store.DBMigFailed && m.Status != store.DBMigCanceled {
		s.mailBack(w, r, back, "error", "Solo se puede borrar la base de una migración fallida o cancelada.")
		return
	}
	if running, err := s.st.RunningDBMigrationTo(m.DstDB); err == nil && running != nil {
		s.mailBack(w, r, back, "error", "Hay otra migración en curso hacia "+m.DstDB+".")
		return
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.mailBack(w, r, back, "error", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := dbmigrate.DropDatabase(ctx, s.mainDB(), m.DstDB); err != nil {
		s.mailBack(w, r, back, "error", "No se pudo borrar: "+err.Error())
		return
	}
	s.mailBack(w, r, back, "ok", "Se borró la base "+m.DstDB+" de MainDB. Ya puedes migrar de nuevo con ese nombre.")
}
