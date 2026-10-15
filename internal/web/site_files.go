package web

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"log"
	"mime"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Explorador de archivos de la consola: navega las carpetas del contenedor activo del sitio
// y descarga archivos (o carpetas completas como .tar.gz). Solo lectura.

// listScript lista una carpeta: la primera línea es la ruta real (pwd) y luego
// "tipo|bytes|unix|nombre" por entrada (stat de coreutils o de busybox).
const listScript = `cd -- "$1" 2>/dev/null || { echo "!"; exit 0; }
pwd
stat -L -c '%F|%s|%Y|%n' -- * .[!.]* ..?* 2>/dev/null
exit 0`

const maxEntries = 2000

type fileEntry struct {
	Name     string
	Path     string
	Dir      bool
	Size     int64
	Modified time.Time
}

type fileCrumb struct{ Name, Path string }

type filesView struct {
	SiteID  int64
	Path    string
	Parent  string // vacío en la raíz
	Crumbs  []fileCrumb
	Entries []fileEntry
	Hidden  int // entradas que no se muestran (carpeta muy grande)
	Error   string
}

// cleanPath normaliza una ruta absoluta del contenedor ("" si no es válida).
func cleanPath(p string) string {
	if p == "" || strings.ContainsRune(p, 0) {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return path.Clean(p)
}

func parseListing(out string) (cwd string, entries []fileEntry, ok bool) {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) == 0 || lines[0] == "!" || !strings.HasPrefix(lines[0], "/") {
		return "", nil, false
	}
	cwd = lines[0]
	for _, l := range lines[1:] {
		f := strings.SplitN(l, "|", 4)
		if len(f) != 4 || f[3] == "" { // los patrones sin coincidencias no imprimen nada (stat falla)
			continue
		}
		size, _ := strconv.ParseInt(f[1], 10, 64)
		mtime, _ := strconv.ParseInt(f[2], 10, 64)
		e := fileEntry{Name: f[3], Path: path.Join(cwd, f[3]), Dir: f[0] == "directory", Size: size}
		if mtime > 0 {
			e.Modified = time.Unix(mtime, 0)
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Dir != entries[j].Dir {
			return entries[i].Dir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	return cwd, entries, true
}

func pathCrumbs(p string) []fileCrumb {
	crumbs := []fileCrumb{{Name: "/", Path: "/"}}
	cur := ""
	for _, part := range strings.Split(strings.Trim(p, "/"), "/") {
		if part == "" {
			continue
		}
		cur += "/" + part
		crumbs = append(crumbs, fileCrumb{Name: part, Path: cur})
	}
	return crumbs
}

// siteFiles devuelve el panel con el contenido de una carpeta (parcial htmx).
func (s *Server) siteFiles(w http.ResponseWriter, r *http.Request) {
	site, ok := s.loadSite(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	v := filesView{SiteID: site.ID}
	cid, err := s.engine.RunningContainer(ctx, site)
	if err != nil {
		v.Error = "El sitio no tiene un contenedor corriendo."
		s.renderPartial(w, "site_files", v)
		return
	}
	dir := cleanPath(r.URL.Query().Get("path"))
	if dir == "" {
		dir = "/"
		if info, err := s.dc.ContainerInspect(ctx, cid); err == nil && info.Config.WorkingDir != "" {
			dir = cleanPath(info.Config.WorkingDir)
		}
	}
	// Como root: el panel muestra todo lo que hay, igual que la consola como root.
	out, _, err := s.dc.ContainerExecAs(ctx, cid, "root", []string{"sh", "-c", listScript, "sh", dir})
	cwd, entries, ok := parseListing(out)
	switch {
	case err != nil:
		v.Error = "No se pudo listar: " + err.Error()
		v.Path = dir
	case !ok:
		v.Error = "No se puede abrir " + dir + "."
		v.Path = dir
	default:
		v.Path = cwd
		if len(entries) > maxEntries {
			v.Hidden = len(entries) - maxEntries
			entries = entries[:maxEntries]
		}
		v.Entries = entries
	}
	if v.Path != "/" {
		v.Parent = path.Dir(v.Path)
	}
	v.Crumbs = pathCrumbs(v.Path)
	s.renderPartial(w, "site_files", v)
}

// siteFileDownload descarga un archivo del contenedor, o una carpeta como .tar.gz.
func (s *Server) siteFileDownload(w http.ResponseWriter, r *http.Request) {
	site, ok := s.loadSite(w, r)
	if !ok {
		return
	}
	p := cleanPath(r.URL.Query().Get("path"))
	if p == "" {
		http.Error(w, "Ruta inválida", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	cid, err := s.engine.RunningContainer(ctx, site)
	if err != nil {
		http.Error(w, "El sitio no tiene un contenedor corriendo", http.StatusConflict)
		return
	}
	st, err := s.dc.ArchiveStat(ctx, cid, p)
	if err == nil && st.LinkTarget != "" { // enlace simbólico: se descarga a lo que apunta
		p = cleanPath(st.LinkTarget)
		st, err = s.dc.ArchiveStat(ctx, cid, p)
	}
	if err != nil {
		http.Error(w, "No existe o no se puede leer: "+p, http.StatusNotFound)
		return
	}
	body, err := s.dc.ArchiveGet(ctx, cid, p)
	if err != nil {
		http.Error(w, "No se pudo descargar: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer body.Close()
	name := path.Base(p)
	if name == "/" || name == "." {
		name = "raiz"
	}
	log.Printf("archivos: %s descargó %s de %s (IP %s)", currentUser(r).Email, p, site.Name, clientIP(r))

	if st.Mode.IsDir() {
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name + ".tar.gz"}))
		gz := gzip.NewWriter(w)
		if _, err := io.Copy(gz, body); err != nil {
			log.Printf("archivos: descarga de %s cortada: %v", p, err)
		}
		gz.Close()
		return
	}
	tr := tar.NewReader(body)
	hdr, err := tr.Next()
	if err != nil {
		http.Error(w, "No se pudo leer el archivo: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("Content-Length", strconv.FormatInt(hdr.Size, 10))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	io.Copy(w, tr)
}
