package web

import (
	"log"
	"net/http"
	"strings"

	"world/internal/store"
)

// phpUpdatable: sitios PHP cuyo contenedor corre una versión anterior a la que ya trae la imagen base.
func (s *Server) phpUpdatable() []*store.Site {
	if s.php == nil {
		return nil
	}
	sites, err := s.st.Sites()
	if err != nil {
		return nil
	}
	var out []*store.Site
	for _, site := range sites {
		if st, ok := s.php.Site(site); ok && st.NeedsDeploy() {
			out = append(out, site)
		}
	}
	return out
}

// phpUpdate lanza un deploy de cada sitio PHP desactualizado (cada uno según su modo de deploy).
func (s *Server) phpUpdate(w http.ResponseWriter, r *http.Request) {
	var started, failed []string
	for _, site := range s.phpUpdatable() {
		if _, err := s.engine.Start(site.ID, store.SourceManual); err != nil {
			failed = append(failed, site.Name+" ("+err.Error()+")")
			continue
		}
		started = append(started, site.Name)
	}
	log.Printf("php: %s lanzó el deploy de %v (IP %s)", currentUser(r).Email, started, clientIP(r))
	switch {
	case len(started) == 0 && len(failed) == 0:
		s.setFlash(w, "ok", "No hay sitios PHP por actualizar.")
	case len(failed) > 0:
		s.setFlash(w, "error", "Deploy lanzado: "+strings.Join(started, ", ")+". No se pudo: "+strings.Join(failed, "; "))
	default:
		s.setFlash(w, "ok", "Deploy lanzado en "+strings.Join(started, ", ")+". Sigue el avance en cada sitio.")
	}
	redirect(w, r, "/")
}
