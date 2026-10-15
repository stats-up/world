package web

import (
	"html/template"
	"strings"
	"testing"
	"time"

	"world/internal/dbmigrate"
	"world/internal/mail"
	"world/internal/store"
)

func TestTemplatesParse(t *testing.T) {
	s := &Server{pages: map[string]*template.Template{}}
	if err := s.parseTemplates(); err != nil {
		t.Fatal(err)
	}
}

func TestSettingsPageRenders(t *testing.T) {
	s := &Server{pages: map[string]*template.Template{}}
	if err := s.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	ms := mail.Settings{Hostname: "mx.statsup.cl", S3Prefix: "mail/", CFToken: "x"}
	for _, st := range []mail.Status{{State: "no instalado"}, {State: "running", Health: "healthy"}, {State: "exited", OOM: true}} {
		var b strings.Builder
		d := data{"CSRF": "tok", "Port": "9000", "Mail": s.mailSettingsData(ms), "MailStatus": st}
		if err := s.pages["settings"].ExecuteTemplate(&b, "layout", d); err != nil {
			t.Fatal(err)
		}
		out := b.String()
		for _, want := range []string{`action="/settings/mail"`, "mx.statsup.cl", "token guardado", "Para activarlo falta"} {
			if !strings.Contains(out, want) {
				t.Errorf("estado %v: falta %q", st, want)
			}
		}
		if strings.Contains(out, `value="x"`) {
			t.Error("el token no debe aparecer en la página")
		}
	}
}

func TestMailPagesRender(t *testing.T) {
	s := &Server{pages: map[string]*template.Template{}}
	if err := s.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	dom := &mail.Domain{ID: "c", Name: "prueba.frikiforja.cl", Origin: "frikiforja.cl"}
	acc := mail.Account{ID: "a", Name: "test", Email: "test@prueba.frikiforja.cl", Quota: 5 << 30, Used: 1 << 30, Aliases: []string{"info"}}
	recs := []mail.Record{{Name: "prueba.frikiforja.cl", Type: "MX", Value: "10 mx.statsup.cl", Status: "ok"},
		{Name: "prueba.frikiforja.cl", Type: "TXT", Value: "v=spf1 mx -all", Status: "distinto", Found: "v=spf1 ~all"}}
	running := &store.MailImport{ID: 7, AccountID: "a", Host: "38.18.230.11", Port: 993, SrcUser: acc.Email, Status: store.ImportRunning, Processed: 32, Copied: 30, Total: 40, StartedAt: time.Now()}
	done := &store.MailImport{ID: 6, AccountID: "a", Host: "38.18.230.11", Port: 993, SrcUser: acc.Email, Status: store.ImportSuccess, Processed: 40, Copied: 40, Total: 40, Bytes: 3 << 20,
		StartedAt: time.Now().Add(-time.Hour), FinishedAt: time.Now()}
	pages := map[string]data{
		"mail":                {"Domains": []mail.Domain{*dom}, "Hostname": "mx.statsup.cl", "Used": int64(3 << 30), "AccountsTotal": 2, "ActiveCount": 0, "MigrationCount": 1},
		"mail_domain":         {"Domain": dom, "Accounts": []mail.Account{acc}, "Tab": "ajustes", "Hostname": "mx.statsup.cl"},
		"mail_domain#buzones": {"Domain": dom, "Accounts": []mail.Account{acc}, "Tab": "buzones", "Hostname": "mx.statsup.cl"},
		"mail_domain#dns":     {"Domain": dom, "Accounts": []mail.Account(nil), "Records": recs, "Pending": 1, "Tab": "dns", "Hostname": "mx.statsup.cl"},
		"mail_domain_new":     {"Hostname": "mx.statsup.cl", "Form": map[string]string{"mode": "migration"}, "Error": "Dominio inválido."},
		"mail_account_new":    {"Domain": dom, "Form": map[string]string{"quota_gb": "5"}},
		"mail_account":        {"Domain": dom, "Account": &acc, "QuotaGB": "5", "Aliases": "info", "Hostname": "mx.statsup.cl"},
		"mail_account#import": {"Domain": dom, "Account": &acc, "QuotaGB": "5", "Hostname": "mx.statsup.cl", "Imports": []*store.MailImport{running, done},
			"Import": data{"Import": running, "Base": "/mail/domains/c/accounts/a", "Log": "ETA: x  1 s  10/40 msgs left", "CSRF": "tok"}},
		"mail_account#synced": {"Domain": dom, "Account": &acc, "QuotaGB": "5", "Hostname": "mx.statsup.cl", "Imports": []*store.MailImport{done},
			"LastImport": data{"Import": done, "Base": "/mail/domains/c/accounts/a", "CSRF": "tok"}, "ImportForm": map[string]string{"host": "38.18.230.11", "port": "993", "user": acc.Email}},
		"mail_password": {"Domain": dom, "Email": acc.Email, "Password": "Secreto123", "Created": true, "Hostname": "mx.statsup.cl"},
	}
	for name, d := range pages {
		d["CSRF"] = "tok"
		var b strings.Builder
		page, _, _ := strings.Cut(name, "#")
		if err := s.pages[page].ExecuteTemplate(&b, "layout", d); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for want, in := range map[string]string{
			`href="/mail/domains/c/accounts/a"`:                   "mail_domain#buzones",
			`data-filter="#accounts"`:                             "mail_domain#buzones",
			"10 mx.statsup.cl":                                    "mail_domain#dns",
			`action="/mail/domains"`:                              "mail_domain_new",
			`action="/mail/domains/c/accounts"`:                   "mail_account_new",
			`href="/mail/domains/c/accounts/new"`:                 "mail_domain",
			`hx-get="/mail/domains/c/accounts/a/import/7"`:        "mail_account#import",
			`action="/mail/domains/c/accounts/a/import/7/cancel"`: "mail_account#import",
			"80% <small>32 de 40</small>":                         "mail_account#import",
			`action="/mail/domains/c/accounts/a/import"`:          "mail_account#synced",
			`hx-post="/mail/domains/c/accounts/a/import/test"`:    "mail_account#synced",
			"Sincronizar de nuevo":                                "mail_account#synced",
			"40 <small>3 MB</small>":                              "mail_account#synced",
			`value="38.18.230.11"`:                                "mail_account#synced",
		} {
			if in == name && !strings.Contains(b.String(), want) {
				t.Errorf("%s: falta %q", name, want)
			}
		}
		if strings.Contains(b.String(), "style=\"") {
			t.Errorf("%s: estilos inline (los bloquea la CSP)", name)
		}
		if name == "mail_domain" && !strings.Contains(b.String(), `data-confirm="¿Eliminar el dominio prueba.frikiforja.cl?"`) {
			t.Errorf("mail_domain: falta la confirmación de eliminar")
		}
	}
}

func TestBucketPagesRender(t *testing.T) {
	s := &Server{pages: map[string]*template.Template{}}
	if err := s.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	b := &store.Bucket{ID: 3, Name: "Archivos", Bucket: "bucket-statsup", Region: "sa-east-1", ScanStatus: store.ScanOK,
		ScannedAt: time.Now(), TotalBytes: 3 << 30, TotalObjects: 1200}
	failed := *b
	failed.ScanStatus, failed.ScanError = store.ScanFailed, "las llaves no tienen permiso"
	rows := []folderRow{{Name: "fotos/", Prefix: "docs/fotos/", Bytes: 2 << 30, Objects: 1000, Pct: 67},
		{Bytes: 1 << 30, Objects: 200, Pct: 33}}
	pages := map[string]data{
		"buckets": {"Buckets": []struct {
			B   *store.Bucket
			Pct int
		}{{b, 100}}, "Total": b.TotalBytes},
		"bucket_new": {"Form": map[string]string{"region": "sa-east-1"}, "Error": "No se pudo leer el bucket"},
		"bucket": {"Bucket": b, "Prefix": "docs/", "Crumbs": crumbs("docs/"), "Scan": data{"Bucket": b, "Running": false},
			"Folder": &store.BucketFolder{Prefix: "docs/", Bytes: 3 << 30, Objects: 1200}, "Rows": rows, "Hidden": 4},
		"bucket#escaneando": {"Bucket": &failed, "Prefix": "", "Scan": data{"Bucket": &failed, "Running": true, "Counted": int64(5000)}},
	}
	for name, d := range pages {
		page, _, _ := strings.Cut(name, "#")
		d["CSRF"] = "tok"
		var out strings.Builder
		if err := s.pages[page].ExecuteTemplate(&out, "layout", d); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		html := out.String()
		switch name {
		case "bucket":
			for _, want := range []string{"2.0 GB", "Archivos sueltos en esta carpeta", `href="/buckets/3?p=docs%2ffotos%2f"`, "4 carpetas más"} {
				if !strings.Contains(html, want) {
					t.Errorf("bucket: falta %q", want)
				}
			}
			if strings.Index(html, "fotos/") > strings.Index(html, "Archivos sueltos") {
				t.Error("las filas deben quedar en el orden recibido (de mayor a menor)")
			}
		case "bucket#escaneando":
			if !strings.Contains(html, "5.000 objetos contados") || !strings.Contains(html, `hx-get="/buckets/3/status"`) {
				t.Error("debe mostrar el avance y actualizarse solo")
			}
		}
	}
}

func TestDatabasePagesRender(t *testing.T) {
	s := &Server{pages: map[string]*template.Template{}}
	if err := s.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	running := &store.DBMigration{ID: 4, SrcHost: "38.18.230.11", SrcPort: 3306, SrcUser: "cp_u", SrcDB: "cp_tienda", DstDB: "tienda",
		Status: store.DBMigRunning, TablesTotal: 40, TablesDone: 10, BytesEst: 300 << 20, StartedAt: now}
	failed := *running
	failed.Status, failed.Error, failed.FinishedAt = store.DBMigFailed, "Error 1419: algo", now
	main := dbmigrate.Conn{Host: "ls-x.rds.amazonaws.com", Port: 3306, User: "dbmasteruser"}
	pages := map[string]data{
		"databases":          {"MainDB": main, "MainReady": true, "Migrations": []*store.DBMigration{running, &failed}, "Form": map[string]string{"src_port": "3306"}},
		"databases#empty":    {"MainDB": dbmigrate.Conn{Port: 3306}, "MainReady": false, "Form": map[string]string{}, "Error": "Falta algo"},
		"db_migration":       {"Mig": running, "Status": data{"Mig": running, "Log": "-- Table structure for table `a`", "CSRF": "tok"}},
		"db_migration#live": {"Mig": running, "Status": data{"Mig": running, "CSRF": "tok", "Live": &dbmigrate.Live{Table: "request_applicant_service_files",
			Rows: 1234567, RowsEst: 2500000, Received: 13 << 30, Idle: 30 * time.Minute, DBState: "waiting for handler commit", DBTime: 1800}}},
		"db_migration#error": {"Mig": &failed, "Status": data{"Mig": &failed, "Log": "ERROR 1419", "CSRF": "tok"}},
	}
	for name, d := range pages {
		page, _, _ := strings.Cut(name, "#")
		d["CSRF"] = "tok"
		var out strings.Builder
		if err := s.pages[page].ExecuteTemplate(&out, "layout", d); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		html := out.String()
		checks := map[string][]string{
			"databases":          {`hx-post="/databases/probe"`, "dbmasteruser@ls-x.rds.amazonaws.com", "cp_tienda", "10 / 40"},
			"databases#empty":    {"Falta algo", "disabled title=\"Primero configura"},
			"db_migration":       {`hx-get="/databases/migrations/4/status"`, "Cancelar", "data-follow"},
			"db_migration#live":  {"request_applicant_service_files", "1.234.567", "2.500.000", "(49%)", "13.0 GB", "detenido", "Sin datos nuevos desde hace 30m0s", "1800 s en «waiting for handler commit»"},
			"db_migration#error": {"Error 1419", `action="/databases/migrations/4/drop"`, "data-confirm="},
		}[name]
		for _, want := range checks {
			if !strings.Contains(html, want) {
				t.Errorf("%s: falta %q", name, want)
			}
		}
	}
	// El resultado de la prueba de conexión (parcial htmx).
	var out strings.Builder
	p := &probeResult{DstDB: "tienda", Source: &dbmigrate.SourceInfo{Version: "10.6.18-MariaDB", Tables: 40, Bytes: 300 << 20, Triggers: 2, Charset: "utf8mb4", Collation: "utf8mb4_general_ci"},
		Target: &dbmigrate.TargetInfo{Version: "8.4.11", Exists: true, Tables: 3}, Blocking: []string{"ya existe"}}
	if err := s.partials.ExecuteTemplate(&out, "db_probe", p); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Origen conectado", "10.6.18-MariaDB", "2 triggers", "ya existe y tiene 3", "ya existe"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("probe: falta %q", want)
		}
	}
}
