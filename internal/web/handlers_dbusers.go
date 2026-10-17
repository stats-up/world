package web

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"world/internal/dbmigrate"
)

// Usuarios de una instancia: un usuario por aplicación con acceso solo a su base. world genera las
// contraseñas y las muestra una sola vez (no las guarda).

// dbUsersData arma la página de usuarios de la instancia.
func (s *Server) dbUsersData(ctx context.Context, v *dbInstanceView, c dbmigrate.Conn, selDB string) data {
	d := data{"Inst": v, "Tab": "usuarios", "Profiles": dbmigrate.Profiles, "SelDB": selDB}
	if sum, err := dbmigrate.Summary(ctx, c); err == nil {
		var dbs []string
		for _, db := range sum.Databases {
			if !db.System {
				dbs = append(dbs, db.Name)
			}
		}
		d["DBs"] = dbs
	}
	users, err := dbmigrate.Users(ctx, c)
	if err != nil {
		d["Error"] = err.Error()
	}
	var app, system []dbmigrate.DBUser
	for _, u := range users {
		if u.System && !u.Self {
			system = append(system, u)
		} else {
			app = append(app, u)
		}
	}
	d["Users"], d["SystemUsers"] = app, system
	return d
}

func (s *Server) dbUsers(w http.ResponseWriter, r *http.Request) {
	v, c, ok := s.instance(r.PathValue("inst"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	s.render(w, r, "db_users", s.dbUsersData(ctx, v, c, r.URL.Query().Get("db")))
}

func usersURL(v *dbInstanceView) string { return "/databases/users/" + v.Key }

// showSecret muestra la contraseña recién generada, una sola vez y sin que quede en caché.
func (s *Server) showSecret(w http.ResponseWriter, r *http.Request, v *dbInstanceView, c dbmigrate.Conn, user, db, pass, title string) {
	w.Header().Set("Cache-Control", "no-store")
	env := []string{"DB_CONNECTION=mysql", "DB_HOST=" + c.Host, "DB_PORT=" + strconv.Itoa(c.Port)}
	if db != "" {
		env = append(env, "DB_DATABASE="+db)
	}
	env = append(env, "DB_USERNAME="+user, "DB_PASSWORD="+pass)
	s.render(w, r, "db_user_secret", data{"Inst": v, "Title": title, "Account": user, "DB": db, "Password": pass, "Env": strings.Join(env, "\n")})
}

func (s *Server) dbUserCreate(w http.ResponseWriter, r *http.Request) {
	v, c, ok := s.instance(r.PathValue("inst"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	user := strings.ToLower(strings.TrimSpace(r.FormValue("user")))
	db := r.FormValue("db")
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	pass, err := dbmigrate.CreateUser(ctx, c, user, db, r.FormValue("profile"))
	if err != nil {
		s.setFlash(w, "error", "No se pudo crear el usuario: "+err.Error())
		redirect(w, r, usersURL(v)+"?db="+url.QueryEscape(db)+"#nuevo")
		return
	}
	log.Printf("bases: %s creó el usuario %s (perfil %s sobre %s) en %s (IP %s)", currentUser(r).Email, user, r.FormValue("profile"), db, v.Name, clientIP(r))
	s.showSecret(w, r, v, c, user, db, pass, "Usuario "+user+" creado")
}

func (s *Server) dbUserGrant(w http.ResponseWriter, r *http.Request) {
	v, c, ok := s.instance(r.PathValue("inst"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	user, host, db, profile := r.FormValue("user"), r.FormValue("host"), r.FormValue("db"), r.FormValue("profile")
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := dbmigrate.Grant(ctx, c, user, host, db, profile); err != nil {
		s.setFlash(w, "error", "No se pudo dar el acceso: "+err.Error())
	} else {
		log.Printf("bases: %s dio el perfil %s sobre %s a %s@%s en %s (IP %s)", currentUser(r).Email, profile, db, user, host, v.Name, clientIP(r))
		label := profile
		if p := dbmigrate.ProfileByKey(profile); p != nil {
			label = p.Label
		}
		s.setFlash(w, "ok", user+" ahora tiene acceso «"+label+"» a "+db+".")
	}
	redirect(w, r, usersURL(v))
}

func (s *Server) dbUserRevoke(w http.ResponseWriter, r *http.Request) {
	v, c, ok := s.instance(r.PathValue("inst"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	user, host, db := r.FormValue("user"), r.FormValue("host"), r.FormValue("db")
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := dbmigrate.Revoke(ctx, c, user, host, db); err != nil {
		s.setFlash(w, "error", "No se pudo quitar el acceso: "+err.Error())
	} else {
		log.Printf("bases: %s quitó el acceso de %s@%s a %s en %s (IP %s)", currentUser(r).Email, user, host, db, v.Name, clientIP(r))
		s.setFlash(w, "ok", "Acceso quitado.")
	}
	redirect(w, r, usersURL(v))
}

func (s *Server) dbUserPassword(w http.ResponseWriter, r *http.Request) {
	v, c, ok := s.instance(r.PathValue("inst"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	user, host := r.FormValue("user"), r.FormValue("host")
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	pass, err := dbmigrate.ResetPassword(ctx, c, user, host)
	if err != nil {
		s.setFlash(w, "error", "No se pudo cambiar la contraseña: "+err.Error())
		redirect(w, r, usersURL(v))
		return
	}
	log.Printf("bases: %s cambió la contraseña de %s@%s en %s (IP %s)", currentUser(r).Email, user, host, v.Name, clientIP(r))
	s.showSecret(w, r, v, c, user, "", pass, "Contraseña nueva para "+user)
}

func (s *Server) dbUserDelete(w http.ResponseWriter, r *http.Request) {
	v, c, ok := s.instance(r.PathValue("inst"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	user, host := r.FormValue("user"), r.FormValue("host")
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := dbmigrate.DropUser(ctx, c, user, host); err != nil {
		s.setFlash(w, "error", "No se pudo eliminar: "+err.Error())
	} else {
		log.Printf("bases: %s eliminó el usuario %s@%s en %s (IP %s)", currentUser(r).Email, user, host, v.Name, clientIP(r))
		s.setFlash(w, "ok", "Usuario "+user+" eliminado. Sus bases y datos no se tocaron.")
	}
	redirect(w, r, usersURL(v))
}
