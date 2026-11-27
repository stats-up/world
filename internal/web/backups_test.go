package web

import (
	"html/template"
	"strings"
	"testing"
	"time"

	"world/internal/backup"
	"world/internal/store"
)

func TestBackupTemplates(t *testing.T) {
	s := &Server{pages: map[string]*template.Template{}}
	if err := s.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	loc, _ := time.LoadLocation("America/Santiago")
	start := time.Date(2026, 11, 27, 3, 0, 5, 0, loc)
	list := []*store.Backup{
		{ID: 9, SiteID: 4, DBName: "tienda", Source: "auto", Status: store.BackupOK, Bytes: 4096, File: "tienda-20261127-0300.sql.gz",
			OnDisk: true, BucketKey: "world-backups/tienda/tienda-20261127-0300.sql.gz", StartedAt: start, FinishedAt: start.Add(12 * time.Second)},
		{ID: 8, SiteID: 4, DBName: "tienda", Source: "manual", Status: store.BackupFailed, Error: "Access denied", StartedAt: start.Add(-time.Hour)},
	}
	v := backupsView{SiteID: 4, Backups: list, Loc: loc, CSRF: "t"}
	if v.Polling() {
		t.Error("sin nada en curso no debe refrescarse")
	}
	cfg := store.DefaultBackupConfig(4)
	cfg.Enabled, cfg.Days, cfg.Hours = true, "135", []int{3, 15}
	site := &store.Site{ID: 4, Name: "tienda"}
	d := data{"Site": site, "Cfg": cfg, "Days": backupDayNames, "Now": start, "DiskUsage": int64(8192), "List": v, "Backups": list, "CSRF": "t",
		"Target": backup.Target{Host: "db.example.com", Port: 3306, Database: "tienda", User: "app"}}
	var out strings.Builder
	if err := s.pages["site_backups"].ExecuteTemplate(&out, "layout", d); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{`name="hours" value="3, 15"`, `value="1" checked`, `value="3" checked`, `value="disk" checked`,
		"2026-11-27 03:00", "programado", "disco + bucket", `action="/sites/4/backups/9/restore"`, `data-confirm-text="Se reemplazan las tablas de la base tienda`,
		`action="/sites/4/backups/8/delete"`, "Access denied", "activos"} {
		if !strings.Contains(html, want) {
			t.Errorf("falta %q", want)
		}
	}
	if strings.Contains(html, `value="2" checked`) || strings.Contains(html, "/backups/8/restore") || strings.Contains(html, "every 3s") {
		t.Error("martes, restaurar uno fallido o refresco de más")
	}
	v.Busy = "backup"
	out.Reset()
	if err := s.partials.ExecuteTemplate(&out, "backup_list", v); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `hx-get="/sites/4/backups/list"`) {
		t.Error("en curso debe refrescarse")
	}
}
