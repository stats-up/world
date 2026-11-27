package web

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"world/internal/store"
)

// Backups de la base de datos de un sitio: configuración, respaldo a pedido, descarga,
// restauración y eliminación. Se activan por sitio; desactivados no ocupan espacio.

var backupDayNames = []struct {
	N    int
	Num  string
	Name string
}{{1, "1", "Lun"}, {2, "2", "Mar"}, {3, "3", "Mié"}, {4, "4", "Jue"}, {5, "5", "Vie"}, {6, "6", "Sáb"}, {7, "7", "Dom"}}

// RunBackups arranca el programador de respaldos (cada minuto, hora de Chile).
func (s *Server) RunBackups(ctx context.Context) { s.backups.Run(ctx) }

func (s *Server) backupsData(site *store.Site, cfg *store.BackupConfig, errMsg string) data {
	d := data{"Site": site, "Cfg": cfg, "Days": backupDayNames, "Busy": s.backups.Busy(site.ID),
		"DiskUsage": s.backups.DiskUsage(site.ID), "Error": errMsg, "Now": time.Now().In(s.backups.Loc)}
	if t, err := s.backups.Target(site); err != nil {
		d["TargetError"] = err.Error()
	} else {
		d["Target"] = t
	}
	d["Buckets"], _ = s.st.Buckets()
	list, err := s.st.Backups(site.ID, 200)
	if err != nil {
		d["ListError"] = err.Error()
	}
	d["Backups"] = list
	return d
}

// backupsView es lo que necesita la tabla (se refresca sola mientras hay algo en curso).
type backupsView struct {
	SiteID  int64
	Backups []*store.Backup
	Busy    string
	Loc     *time.Location
	CSRF    string
}

// Polling: la tabla se refresca sola mientras hay un respaldo o una restauración en curso.
func (v backupsView) Polling() bool {
	if v.Busy != "" {
		return true
	}
	for _, b := range v.Backups {
		if b.Running() {
			return true
		}
	}
	return false
}

func (v backupsView) Local(t time.Time) string { return t.In(v.Loc).Format("2006-01-02 15:04") }

func (s *Server) siteBackups(w http.ResponseWriter, r *http.Request) {
	site, ok := s.loadSite(w, r)
	if !ok {
		return
	}
	cfg, err := s.st.BackupConfig(site.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderBackups(w, r, site, cfg, "")
}

func (s *Server) renderBackups(w http.ResponseWriter, r *http.Request, site *store.Site, cfg *store.BackupConfig, errMsg string) {
	d := s.backupsData(site, cfg, errMsg)
	list, _ := d["Backups"].([]*store.Backup)
	d["List"] = backupsView{SiteID: site.ID, Backups: list, Busy: s.backups.Busy(site.ID), Loc: s.backups.Loc, CSRF: sessionCSRF(r)}
	s.render(w, r, "site_backups", d)
}

// siteBackupsList es la tabla sola (htmx la vuelve a pedir mientras corre un respaldo).
func (s *Server) siteBackupsList(w http.ResponseWriter, r *http.Request) {
	site, ok := s.loadSite(w, r)
	if !ok {
		return
	}
	list, _ := s.st.Backups(site.ID, 200)
	v := backupsView{SiteID: site.ID, Backups: list, Busy: s.backups.Busy(site.ID), Loc: s.backups.Loc}
	v.CSRF = sessionCSRF(r)
	s.renderPartial(w, "backup_list", v)
}

func (s *Server) siteBackupsConfig(w http.ResponseWriter, r *http.Request) {
	site, ok := s.loadSite(w, r)
	if !ok {
		return
	}
	cfg := store.DefaultBackupConfig(site.ID)
	cfg.Enabled = r.FormValue("enabled") == "1"
	cfg.Dest = r.FormValue("dest")
	cfg.BucketID, _ = strconv.ParseInt(r.FormValue("bucket_id"), 10, 64)
	cfg.Days = ""
	for _, d := range backupDayNames {
		if r.Form["days"] != nil && oneOf(d.Num, r.Form["days"]...) {
			cfg.Days += d.Num
		}
	}
	var errs []string
	hours, err := store.ParseBackupHours(r.FormValue("hours"))
	if err != nil {
		errs = append(errs, err.Error())
		cfg.Hours = nil
	} else {
		cfg.Hours = hours
	}
	cfg.Keep, err = strconv.Atoi(strings.TrimSpace(r.FormValue("keep")))
	if err != nil || cfg.Keep < 1 || cfg.Keep > store.MaxBackupKeep {
		errs = append(errs, fmt.Sprintf("La cantidad de copias debe ser un número entre 1 y %d.", store.MaxBackupKeep))
	}
	if cfg.Days == "" {
		errs = append(errs, "Elige al menos un día.")
	}
	switch cfg.Dest {
	case store.BackupDisk:
		cfg.BucketID = 0
	case store.BackupBucket, store.BackupBoth:
		if cfg.BucketID == 0 {
			errs = append(errs, "Elige el bucket donde se guardan las copias (se agregan en la sección Buckets).")
		}
	default:
		errs = append(errs, "Destino inválido.")
	}
	if cfg.Enabled && len(errs) == 0 {
		if _, err := s.backups.Target(site); err != nil {
			errs = append(errs, "No se puede respaldar este sitio: "+err.Error())
		}
	}
	if len(errs) == 0 && cfg.BucketID > 0 {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		err := s.backups.CheckBucket(ctx, cfg.BucketID)
		cancel()
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		w.WriteHeader(http.StatusUnprocessableEntity)
		s.renderBackups(w, r, site, cfg, strings.Join(errs, " "))
		return
	}
	if err := s.st.SaveBackupConfig(cfg); err != nil {
		s.setFlash(w, "error", err.Error())
	} else if cfg.Enabled {
		s.setFlash(w, "ok", "Backups activados.")
	} else {
		s.setFlash(w, "ok", "Configuración guardada. Los backups programados están desactivados.")
	}
	redirect(w, r, fmt.Sprintf("/sites/%d/backups", site.ID))
}

func (s *Server) siteBackupRun(w http.ResponseWriter, r *http.Request) {
	site, ok := s.loadSite(w, r)
	if !ok {
		return
	}
	if _, err := s.backups.Start(site, "manual"); err != nil {
		s.setFlash(w, "error", err.Error())
	} else {
		s.setFlash(w, "ok", "Respaldo iniciado. Aparece en la lista cuando termine.")
	}
	redirect(w, r, fmt.Sprintf("/sites/%d/backups", site.ID))
}

// loadBackup busca el respaldo y confirma que es del sitio de la URL.
func (s *Server) loadBackup(w http.ResponseWriter, r *http.Request) (*store.Site, *store.Backup, bool) {
	site, ok := s.loadSite(w, r)
	if !ok {
		return nil, nil, false
	}
	id, _ := strconv.ParseInt(r.PathValue("bid"), 10, 64)
	b, err := s.st.Backup(id)
	if err != nil || b.SiteID != site.ID {
		http.NotFound(w, r)
		return nil, nil, false
	}
	return site, b, true
}

func (s *Server) siteBackupDownload(w http.ResponseWriter, r *http.Request) {
	_, b, ok := s.loadBackup(w, r)
	if !ok {
		return
	}
	if !b.OK() {
		http.Error(w, "este respaldo no tiene archivo", http.StatusNotFound)
		return
	}
	body, size, err := s.backups.Open(r.Context(), b)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+b.File+`"`)
	if size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	if _, err := io.Copy(w, body); err != nil {
		log.Printf("descargando backup %d: %v", b.ID, err)
	}
}

func (s *Server) siteBackupRestore(w http.ResponseWriter, r *http.Request) {
	site, b, ok := s.loadBackup(w, r)
	if !ok {
		return
	}
	if err := s.backups.Restore(site, b); err != nil {
		s.setFlash(w, "error", err.Error())
	} else {
		log.Printf("restaurando backup %d de %s (base %s)", b.ID, site.Name, b.DBName)
		s.setFlash(w, "ok", "Restauración iniciada: primero se respalda el estado actual y luego se cargan las tablas del respaldo. El resultado aparece en la lista.")
	}
	redirect(w, r, fmt.Sprintf("/sites/%d/backups", site.ID))
}

func (s *Server) siteBackupDelete(w http.ResponseWriter, r *http.Request) {
	site, b, ok := s.loadBackup(w, r)
	if !ok {
		return
	}
	if b.Running() || s.backups.Busy(site.ID) == "restore" {
		s.setFlash(w, "error", "Espera a que termine el respaldo o la restauración en curso.")
		redirect(w, r, fmt.Sprintf("/sites/%d/backups", site.ID))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	if err := s.backups.Remove(ctx, b); err != nil {
		s.setFlash(w, "error", "No se pudo eliminar: "+err.Error())
	} else {
		s.setFlash(w, "ok", "Respaldo eliminado.")
	}
	redirect(w, r, fmt.Sprintf("/sites/%d/backups", site.ID))
}
