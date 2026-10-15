package web

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"world/internal/bucket"
	"world/internal/store"
)

// Buckets: world mide el espacio usado por bucket y por carpeta (solo lectura).

var (
	s3BucketRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	regionRe   = regexp.MustCompile(`^[a-z0-9-]{2,30}$`)
)

// Filas mostradas por carpeta; el resto se resume en una línea.
const folderRows = 300

// RunBucketScans retoma lo interrumpido y vuelve a medir los buckets una vez al día.
func (s *Server) RunBucketScans(ctx context.Context) { s.buckets.Run(ctx) }

func (s *Server) bucketIndex(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.Buckets()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var total int64
	for _, b := range list {
		total += b.TotalBytes
	}
	type row struct {
		B   *store.Bucket
		Pct int // parte del total de todos los buckets
	}
	rows := make([]row, len(list))
	for i, b := range list {
		rows[i] = row{b, sharePct(b.TotalBytes, total)}
	}
	s.render(w, r, "buckets", data{"Buckets": rows, "Total": total})
}

func (s *Server) bucketNew(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "bucket_new", data{"Form": map[string]string{"region": "sa-east-1"}})
}

func (s *Server) bucketCreate(w http.ResponseWriter, r *http.Request) {
	form := map[string]string{}
	for _, k := range []string{"name", "bucket", "region", "endpoint", "access_key"} {
		form[k] = strings.TrimSpace(r.FormValue(k))
	}
	form["bucket"], form["region"] = strings.ToLower(form["bucket"]), strings.ToLower(form["region"])
	secretKey := strings.TrimSpace(r.FormValue("secret_key"))
	fail := func(msg string) {
		s.render(w, r, "bucket_new", data{"Form": form, "Error": msg})
	}
	switch {
	case !s3BucketRe.MatchString(form["bucket"]):
		fail("Nombre de bucket inválido: minúsculas, números, puntos y guiones.")
		return
	case !regionRe.MatchString(form["region"]):
		fail("Región inválida (ej: sa-east-1).")
		return
	case form["endpoint"] != "" && !validEndpoint(form["endpoint"]):
		fail("El endpoint debe ser una URL https:// (ej: https://<cuenta>.r2.cloudflarestorage.com).")
		return
	case form["access_key"] == "" || secretKey == "":
		fail("Faltan las llaves de acceso.")
		return
	}
	if form["name"] == "" {
		form["name"] = form["bucket"]
	}
	c := bucket.Creds{Bucket: form["bucket"], Region: form["region"], Endpoint: form["endpoint"],
		AccessKey: form["access_key"], SecretKey: secretKey}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := s.buckets.Check(ctx, c); err != nil {
		fail("No se pudo leer el bucket: " + err.Error())
		return
	}
	id, err := s.st.CreateBucket(&store.Bucket{Name: form["name"], Bucket: c.Bucket, Region: c.Region, Endpoint: c.Endpoint,
		AccessKey: c.AccessKey, SecretEnc: s.box.SealString(secretKey)})
	if err != nil {
		fail("No se pudo guardar: " + err.Error())
		return
	}
	s.buckets.Start(id)
	s.setFlash(w, "ok", "Bucket agregado. Midiendo su espacio…")
	redirect(w, r, "/buckets/"+strconv.FormatInt(id, 10))
}

func validEndpoint(e string) bool {
	u, err := url.Parse(e)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.RawQuery == ""
}

func (s *Server) loadBucket(w http.ResponseWriter, r *http.Request) (*store.Bucket, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return nil, false
	}
	b, err := s.st.Bucket(id)
	if err != nil {
		http.NotFound(w, r)
		return nil, false
	}
	return b, true
}

// folderRow es una fila del explorador: una subcarpeta o los archivos sueltos de la carpeta.
type folderRow struct {
	Name    string
	Prefix  string // vacío en la fila de archivos sueltos
	Bytes   int64
	Objects int64
	Pct     int
}

type crumb struct{ Name, Prefix string }

func (s *Server) bucketShow(w http.ResponseWriter, r *http.Request) {
	b, ok := s.loadBucket(w, r)
	if !ok {
		return
	}
	prefix := r.URL.Query().Get("p")
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	d := data{"Bucket": b, "Prefix": prefix, "Crumbs": crumbs(prefix), "Scan": s.scanData(b)}
	if !b.ScannedAt.IsZero() {
		cur, err := s.st.BucketFolder(b.ID, prefix)
		if err != nil {
			d["Missing"] = true // la carpeta ya no existe en el último escaneo
		} else {
			subs, total, err := s.st.BucketSubfolders(b.ID, prefix, folderRows)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			rows := make([]folderRow, 0, len(subs)+1)
			for _, f := range subs {
				rows = append(rows, folderRow{Name: strings.TrimPrefix(f.Prefix, prefix), Prefix: f.Prefix,
					Bytes: f.Bytes, Objects: f.Objects, Pct: sharePct(f.Bytes, cur.Bytes)})
			}
			if cur.Files > 0 {
				rows = append(rows, folderRow{Bytes: cur.FileBytes, Objects: cur.Files, Pct: sharePct(cur.FileBytes, cur.Bytes)})
			}
			// Los archivos sueltos van en su lugar según su tamaño, igual que las carpetas.
			sort.SliceStable(rows, func(i, j int) bool { return rows[i].Bytes > rows[j].Bytes })
			d["Folder"], d["Rows"], d["Hidden"] = cur, rows, total-len(subs)
		}
	}
	s.render(w, r, "bucket", d)
}

func crumbs(prefix string) []crumb {
	var out []crumb
	acc := ""
	for _, part := range strings.Split(strings.TrimSuffix(prefix, "/"), "/") {
		if part == "" {
			continue
		}
		acc += part + "/"
		out = append(out, crumb{Name: part, Prefix: acc})
	}
	return out
}

func sharePct(part, total int64) int {
	if total <= 0 {
		return 0
	}
	return int((part*100 + total/2) / total)
}

// scanData alimenta el bloque de estado del escaneo (se actualiza solo mientras corre).
func (s *Server) scanData(b *store.Bucket) data {
	n, running := s.buckets.Progress(b.ID)
	return data{"Bucket": b, "Running": running || b.Scanning(), "Counted": n}
}

func (s *Server) bucketStatus(w http.ResponseWriter, r *http.Request) {
	b, ok := s.loadBucket(w, r)
	if !ok {
		return
	}
	d := s.scanData(b)
	if !d["Running"].(bool) {
		// Terminó: recargar la página para mostrar las carpetas nuevas.
		w.Header().Set("HX-Refresh", "true")
	}
	d["CSRF"] = sessionCSRF(r)
	s.renderPartial(w, "bucket_scan", d)
}

func (s *Server) bucketScan(w http.ResponseWriter, r *http.Request) {
	b, ok := s.loadBucket(w, r)
	if !ok {
		return
	}
	back := "/buckets/" + strconv.FormatInt(b.ID, 10)
	if p := r.FormValue("p"); p != "" {
		back += "?p=" + url.QueryEscape(p)
	}
	if !s.buckets.Start(b.ID) {
		s.mailBack(w, r, back, "error", "Ya se está midiendo este bucket.")
		return
	}
	redirect(w, r, back)
}

func (s *Server) bucketDelete(w http.ResponseWriter, r *http.Request) {
	b, ok := s.loadBucket(w, r)
	if !ok {
		return
	}
	if err := s.st.DeleteBucket(b.ID); err != nil {
		s.mailBack(w, r, "/buckets/"+strconv.FormatInt(b.ID, 10), "error", "No se pudo quitar: "+err.Error())
		return
	}
	s.mailBack(w, r, "/buckets", "ok", "Se quitó "+b.Name+" de world. El bucket y sus archivos no se tocaron.")
}
