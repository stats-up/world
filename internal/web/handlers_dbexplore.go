package web

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"world/internal/dbmigrate"
	"world/internal/store"
)

// Explorador de bases: instancias (MainDB y otras guardadas) → bases → tablas → estructura y datos.
// La lectura corre en una sesión READ ONLY con tiempo máximo; la única escritura es eliminar
// registros por su clave, con confirmación.

// dbInstanceView es una instancia en las páginas del explorador (sin contraseña).
type dbInstanceView struct {
	Key     string // "main" o el id
	ID      int64  // 0 = MainDB
	Name    string
	Host    string
	Port    int
	User    string
	Summary *dbmigrate.ServerSummary
	Err     string
}

func (v *dbInstanceView) Main() bool { return v.ID == 0 }

// UserDBs y Bytes cuentan solo las bases que no son del sistema.
func (v *dbInstanceView) UserDBs() int {
	n := 0
	if v.Summary != nil {
		for _, d := range v.Summary.Databases {
			if !d.System {
				n++
			}
		}
	}
	return n
}

func (v *dbInstanceView) Bytes() int64 {
	var n int64
	if v.Summary != nil {
		for _, d := range v.Summary.Databases {
			if !d.System {
				n += d.Bytes
			}
		}
	}
	return n
}

// instance resuelve la instancia de la URL: "main" o el id de una guardada.
func (s *Server) instance(key string) (*dbInstanceView, dbmigrate.Conn, bool) {
	if key == "main" {
		c := s.mainDB()
		if !dbmigrate.Configured(c) {
			return nil, c, false
		}
		return &dbInstanceView{Key: "main", Name: "MainDB", Host: c.Host, Port: c.Port, User: c.User}, c, true
	}
	id, err := strconv.ParseInt(key, 10, 64)
	if err != nil {
		return nil, dbmigrate.Conn{}, false
	}
	d, err := s.st.DBInstance(id)
	if err != nil {
		return nil, dbmigrate.Conn{}, false
	}
	pass, _ := s.box.OpenString(d.PassEnc)
	return &dbInstanceView{Key: key, ID: d.ID, Name: d.Name, Host: d.Host, Port: d.Port, User: d.User},
		dbmigrate.Conn{Host: d.Host, Port: d.Port, User: d.User, Password: pass}, true
}

// renderExplore muestra la pestaña Explorar: las instancias, cada una medida en paralelo.
func (s *Server) renderExplore(w http.ResponseWriter, r *http.Request, form map[string]string, errMsg string) {
	var views []*dbInstanceView
	conns := map[*dbInstanceView]dbmigrate.Conn{}
	if v, c, ok := s.instance("main"); ok {
		views = append(views, v)
		conns[v] = c
	}
	saved, err := s.st.DBInstances()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, d := range saved {
		if v, c, ok := s.instance(strconv.FormatInt(d.ID, 10)); ok {
			views = append(views, v)
			conns[v] = c
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, v := range views {
		wg.Add(1)
		go func(v *dbInstanceView) {
			defer wg.Done()
			sum, err := dbmigrate.Summary(ctx, conns[v])
			if err != nil {
				v.Err = err.Error()
				return
			}
			v.Summary = sum
		}(v)
	}
	wg.Wait()
	main := s.mainDB()
	main.Password = ""
	if form == nil {
		form = map[string]string{"port": "3306"}
	}
	s.render(w, r, "databases", data{"Tab": "explorar", "Instances": views, "MainDB": main,
		"MainReady": dbmigrate.Configured(s.mainDB()), "Form": form, "Error": errMsg})
}

func (s *Server) dbInstanceCreate(w http.ResponseWriter, r *http.Request) {
	form := map[string]string{}
	for _, k := range []string{"name", "host", "port", "user"} {
		form[k] = strings.TrimSpace(r.FormValue(k))
	}
	port, _ := strconv.Atoi(form["port"])
	c := dbmigrate.Conn{Host: form["host"], Port: port, User: form["user"], Password: r.FormValue("password")}
	switch {
	case !validDBHost(c.Host):
		s.renderExplore(w, r, form, "Host inválido.")
		return
	case port < 1 || port > 65535:
		s.renderExplore(w, r, form, "Puerto inválido.")
		return
	case c.User == "" || c.Password == "":
		s.renderExplore(w, r, form, "Falta el usuario o la contraseña.")
		return
	}
	if form["name"] == "" {
		form["name"] = c.Host
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	sum, err := dbmigrate.Summary(ctx, c)
	if err != nil {
		s.renderExplore(w, r, form, "No se pudo conectar: "+err.Error())
		return
	}
	id, err := s.st.CreateDBInstance(&store.DBInstance{Name: form["name"], Host: c.Host, Port: port, User: c.User,
		PassEnc: s.box.SealString(c.Password)})
	if err != nil {
		s.renderExplore(w, r, form, "No se pudo guardar: "+err.Error())
		return
	}
	s.setFlash(w, "ok", "Instancia agregada ("+sum.Version+").")
	redirect(w, r, "/databases/x/"+strconv.FormatInt(id, 10))
}

func (s *Server) dbInstanceDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.st.DeleteDBInstance(id); err != nil {
		s.setFlash(w, "error", "No se pudo quitar: "+err.Error())
	} else {
		s.setFlash(w, "ok", "Instancia quitada de world. El servidor y sus datos no se tocan.")
	}
	redirect(w, r, "/databases")
}

func (s *Server) dbExploreInstance(w http.ResponseWriter, r *http.Request) {
	v, c, ok := s.instance(r.PathValue("inst"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	sum, err := dbmigrate.Summary(ctx, c)
	if err != nil {
		v.Err = err.Error()
	}
	v.Summary = sum
	s.render(w, r, "db_instance", data{"Inst": v})
}

func (s *Server) dbExploreDatabase(w http.ResponseWriter, r *http.Request) {
	v, c, ok := s.instance(r.PathValue("inst"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	db := r.PathValue("db")
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	tables, err := dbmigrate.Tables(ctx, c, db)
	if errors.Is(err, dbmigrate.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	var total, rows int64
	views := 0
	for _, t := range tables {
		total += t.Bytes()
		rows += t.Rows
		if t.View() {
			views++
		}
	}
	d := data{"Inst": v, "DB": db, "Tables": tables, "Total": total, "Rows": rows, "Views": views, "Count": len(tables) - views}
	if err != nil {
		d["Error"] = err.Error()
	}
	s.render(w, r, "db_database", d)
}

// tableQuery es el estado de la grilla de datos, leído de la URL.
type tableQuery struct {
	dbmigrate.RowQuery
	Tab string
}

// URL arma el enlace a la tabla con cambios sobre el estado actual.
func (q tableQuery) URL(base string, changes ...string) string {
	v := url.Values{}
	if q.Tab != "datos" {
		v.Set("tab", q.Tab)
	}
	if q.Order != "" {
		v.Set("o", q.Order)
		if q.Desc {
			v.Set("d", "1")
		}
	}
	if q.FilterCol != "" && q.FilterVal != "" {
		v.Set("fc", q.FilterCol)
		v.Set("fv", q.FilterVal)
	}
	if q.Limit != 50 {
		v.Set("n", strconv.Itoa(q.Limit))
	}
	if q.Offset > 0 {
		v.Set("p", strconv.Itoa(q.Offset))
	}
	for i := 0; i+1 < len(changes); i += 2 {
		if changes[i+1] == "" {
			v.Del(changes[i])
		} else {
			v.Set(changes[i], changes[i+1])
		}
	}
	if len(v) == 0 {
		return base
	}
	return base + "?" + v.Encode()
}

// SortURL: clic en un encabezado ordena por esa columna (y alterna el sentido) desde la primera página.
func (q tableQuery) SortURL(base, col string) string {
	d := ""
	if q.Order == col && !q.Desc {
		d = "1"
	}
	return q.URL(base, "o", col, "d", d, "p", "")
}

func (q tableQuery) PrevURL(base string) string {
	return q.URL(base, "p", strconv.Itoa(max(0, q.Offset-q.Limit)))
}
func (q tableQuery) NextURL(base string) string {
	return q.URL(base, "p", strconv.Itoa(q.Offset+q.Limit))
}
func (q tableQuery) ClearURL(base string) string { return q.URL(base, "fc", "", "fv", "", "p", "") }
func (q tableQuery) From() int                   { return q.Offset + 1 }

func (s *Server) dbExploreTable(w http.ResponseWriter, r *http.Request) {
	v, c, ok := s.instance(r.PathValue("inst"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	db, table := r.PathValue("db"), r.PathValue("table")
	qs := r.URL.Query()
	q := tableQuery{Tab: "datos"}
	if qs.Get("tab") == "estructura" {
		q.Tab = "estructura"
	}
	q.Offset, _ = strconv.Atoi(qs.Get("p"))
	q.Limit, _ = strconv.Atoi(qs.Get("n"))
	if q.Limit != 100 && q.Limit != 200 {
		q.Limit = 50
	}
	q.Order, q.Desc = qs.Get("o"), qs.Get("d") == "1"
	q.FilterCol, q.FilterVal = qs.Get("fc"), qs.Get("fv")

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	d := data{"Inst": v, "DB": db, "Q": q, "Base": "/databases/x/" + v.Key + "/" + url.PathEscape(db) + "/" + url.PathEscape(table)}
	var t *dbmigrate.TableDetail
	var err error
	if q.Tab == "estructura" {
		t, err = dbmigrate.Table(ctx, c, db, table)
	} else {
		var rows []dbmigrate.Row
		var more bool
		t, rows, more, err = dbmigrate.Rows(ctx, c, db, table, q.RowQuery)
		d["Rows"], d["More"] = rows, more
		d["To"] = q.Offset + len(rows)
	}
	if errors.Is(err, dbmigrate.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if t == nil {
		t = &dbmigrate.TableDetail{TableSummary: dbmigrate.TableSummary{Name: table}}
	}
	if err != nil {
		d["Error"] = err.Error()
	}
	d["Table"] = t
	d["DeleteWarning"] = deleteWarning(t)
	s.render(w, r, "db_table", d)
}

// deleteWarning explica qué pasa con los registros de otras tablas que apuntan a los eliminados.
func deleteWarning(t *dbmigrate.TableDetail) string {
	var cascade, block, null []string
	for _, ref := range t.References {
		switch {
		case ref.Cascade():
			cascade = append(cascade, ref.Table)
		case ref.Blocks():
			block = append(block, ref.Table)
		default:
			null = append(null, ref.Table)
		}
	}
	var out []string
	if len(cascade) > 0 {
		out = append(out, "También se eliminan, en cascada, sus registros relacionados en "+strings.Join(cascade, ", ")+".")
	}
	if len(null) > 0 {
		out = append(out, "En "+strings.Join(null, ", ")+" la referencia queda vacía (NULL).")
	}
	if len(block) > 0 {
		out = append(out, "Si tiene registros relacionados en "+strings.Join(block, ", ")+", la base rechaza la eliminación y no se borra nada.")
	}
	return strings.Join(out, " ")
}

// dbExploreDelete elimina registros por su clave, confirmado antes en la página con Sweet Alert.
func (s *Server) dbExploreDelete(w http.ResponseWriter, r *http.Request) {
	v, c, ok := s.instance(r.PathValue("inst"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	db, table := r.PathValue("db"), r.PathValue("table")
	base := "/databases/x/" + v.Key + "/" + url.PathEscape(db) + "/" + url.PathEscape(table)
	back := r.FormValue("back")
	if back != base && !strings.HasPrefix(back, base+"?") {
		back = base
	}
	keys := r.Form["k"]
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	n, err := dbmigrate.DeleteRows(ctx, c, db, table, keys)
	if err != nil {
		s.setFlash(w, "error", "No se pudo eliminar: "+err.Error())
		redirect(w, r, back)
		return
	}
	log.Printf("bases: %s eliminó %d registro(s) de %s/%s.%s (IP %s): %s", currentUser(r).Email, n, v.Name, db, table,
		clientIP(r), strings.Join(keys, " "))
	switch {
	case n == 0:
		s.setFlash(w, "error", "No se eliminó nada: los registros ya no existían.")
	case int(n) < len(keys):
		s.setFlash(w, "ok", strconv.FormatInt(n, 10)+" de "+strconv.Itoa(len(keys))+" registros eliminados; el resto ya no existía.")
	case n == 1:
		s.setFlash(w, "ok", "Registro eliminado.")
	default:
		s.setFlash(w, "ok", strconv.FormatInt(n, 10)+" registros eliminados.")
	}
	redirect(w, r, back)
}
