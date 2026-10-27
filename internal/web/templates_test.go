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
		"databases":       {"Tab": "migraciones", "MainDB": main, "MainReady": true, "Migrations": []*store.DBMigration{running, &failed}, "Form": map[string]string{"src_port": "3306"}},
		"databases#empty": {"Tab": "migraciones", "MainDB": dbmigrate.Conn{Port: 3306}, "MainReady": false, "Form": map[string]string{}, "Error": "Falta algo"},
		"databases#explore": {"Tab": "explorar", "MainDB": main, "MainReady": true, "Form": map[string]string{"port": "3306"},
			"Instances": []*dbInstanceView{
				{Key: "main", Name: "MainDB", Host: "ls-x", Port: 3306, User: "dbmasteruser", Summary: &dbmigrate.ServerSummary{Version: "8.4.11",
					Databases: []dbmigrate.DBSummary{{Name: "tienda", Bytes: 5 << 20, Tables: 3}, {Name: "mysql", System: true, Bytes: 1 << 30}}}},
				{Key: "2", ID: 2, Name: "cPanel 5s", Host: "38.18.230.11", Port: 3306, User: "u", Err: "no responde"}}},
		"db_instance": {"Collations": dbmigrate.Collations, "Inst": &dbInstanceView{Key: "main", Name: "MainDB", Host: "ls-x", Port: 3306, User: "admin",
			Summary: &dbmigrate.ServerSummary{Version: "8.4.11", Databases: []dbmigrate.DBSummary{{Name: "client_5s", Tables: 40, Rows: 123456, Bytes: 2 << 30}, {Name: "sys", System: true}}}},
			"UsersOK": true, "DBUsers": map[string][]string{"client_5s": {"app_5s"}}},
		"db_database": {"Inst": &dbInstanceView{Key: "main", Name: "MainDB"}, "DB": "client_5s", "Count": 1, "Views": 1, "Rows": int64(10), "Total": int64(2048),
			"Tables": []dbmigrate.TableSummary{{Name: "users", Type: "BASE TABLE", Engine: "InnoDB", Rows: 10, DataBytes: 1024, IndexBytes: 1024}, {Name: "resumen", Type: "VIEW"}},
			"Profiles": dbmigrate.Profiles,
			"Access": []dbmigrate.Access{{User: &dbmigrate.DBUser{User: "app_5s", Host: "%"}, Grant: &dbmigrate.DBGrant{DB: `client\_5s`, Name: "client_5s", Privs: []string{"SELECT"}, Profile: "readonly"}}},
			"GlobalAccess": []dbmigrate.Access{{User: &dbmigrate.DBUser{User: "dbmasteruser", Host: "%", Self: true}}},
			"OtherUsers": []dbmigrate.DBUser{{User: "app_tienda", Host: "%"}}},
		"db_table": {"Inst": &dbInstanceView{Key: "main", Name: "MainDB"}, "DB": "client_5s", "Base": "/databases/x/main/client_5s/users",
			"Q": tableQuery{Tab: "datos", RowQuery: dbmigrate.RowQuery{Limit: 50, Offset: 50, Order: "id", FilterCol: "name", FilterVal: "ana"}},
			"Table": &dbmigrate.TableDetail{TableSummary: dbmigrate.TableSummary{Name: "users", Type: "BASE TABLE"}, DB: "client_5s",
				Columns: []dbmigrate.Column{{Name: "id", Type: "int", DataType: "int", Key: "PRI"}, {Name: "name", Type: "text", DataType: "text"}, {Name: "foto", DataType: "blob"}},
				Indexes: []dbmigrate.Index{{Name: "PRIMARY", Unique: true, Columns: "id"}}},
			"Rows": []dbmigrate.Row{{Key: `["51"]`, Label: "id = 51", Cells: []dbmigrate.Cell{{Text: "51"}, {Text: "Ana <b>", Cut: true}, {Binary: true, Size: 2048}}},
				{Key: `["52"]`, Label: "id = 52", Cells: []dbmigrate.Cell{{Text: "52"}, {Null: true}, {Null: true}}}},
			"More": true, "To": 52, "DeleteWarning": "También se eliminan, en cascada, sus registros relacionados en orders."},
		"db_table#sinclave": {"Inst": &dbInstanceView{Key: "main", Name: "MainDB"}, "DB": "d", "Base": "/databases/x/main/d/log", "Q": tableQuery{Tab: "datos", RowQuery: dbmigrate.RowQuery{Limit: 50}},
			"Table": &dbmigrate.TableDetail{TableSummary: dbmigrate.TableSummary{Name: "log", Type: "BASE TABLE"}, DB: "d", Columns: []dbmigrate.Column{{Name: "msg", DataType: "varchar"}}},
			"Rows":  []dbmigrate.Row{{Cells: []dbmigrate.Cell{{Text: "hola"}}}}, "To": 1},
		"db_table#estructura": {"Inst": &dbInstanceView{Key: "1", ID: 1, Name: "cPanel"}, "DB": "d", "Base": "/databases/x/1/d/v", "Q": tableQuery{Tab: "estructura"},
			"Table": &dbmigrate.TableDetail{TableSummary: dbmigrate.TableSummary{Name: "v", Type: "VIEW"}, Create: "CREATE VIEW v AS select 1",
				Columns: []dbmigrate.Column{{Name: "id", Type: "int", Key: "PRI"}}, Indexes: []dbmigrate.Index{{Name: "PRIMARY", Unique: true, Columns: "id"}}}},
		"db_users": {"Inst": &dbInstanceView{Key: "main", Name: "MainDB", User: "dbmasteruser"}, "Tab": "usuarios", "Profiles": dbmigrate.Profiles, "SelDB": "client_5s",
			"DBs": []string{"client_5s", "tienda"},
			"Users": []dbmigrate.DBUser{{User: "dbmasteruser", Host: "%", Self: true, Global: []string{"SELECT"}},
				{User: "app_5s", Host: "%", Plugin: "caching_sha2_password", Grants: []dbmigrate.DBGrant{{DB: `client\_5s`, Name: "client_5s", Privs: []string{"SELECT", "SHOW VIEW"}, Profile: "readonly"}}}},
			"SystemUsers": []dbmigrate.DBUser{{User: "rdsadmin", Host: "localhost", System: true}}},
		"db_user_secret": {"Inst": &dbInstanceView{Key: "main", Name: "MainDB"}, "Title": "Usuario app_5s creado", "Account": "app_5s", "DB": "client_5s",
			"Password": "Abc123xyz", "Env": "DB_USERNAME=app_5s\nDB_PASSWORD=Abc123xyz"},
		"db_migration": {"Mig": running, "Status": data{"Mig": running, "Log": "-- Table structure for table `a`", "CSRF": "tok"}},
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
			"databases":         {`hx-post="/databases/probe"`, "cp_tienda", "10 / 40"},
			"databases#empty":   {"Falta algo", "disabled title=\"Primero configura"},
			"databases#explore": {`href="/databases/x/main"`, "dbmasteruser@ls-x.rds.amazonaws.com", "8.4.11", "5 MB", "sin conexión", `action="/databases/instances/2/delete"`, "data-confirm=", `action="/databases/instances"`},
			"db_instance":       {"client_5s", "123.456", "2.0 GB", `data-kind="system" hidden`, `action="/databases/x/main/databases"`, "utf8mb4_unicode_ci · la de Laravel", "<code>app_5s</code>"},
			"db_database":       {`href="/databases/x/main/client_5s/users"`, "vista", "2 KB", `href="/databases/users/main?db=client_5s#nuevo"`, `name="back_db" value="client_5s"`, `value="client\_5s"`, "Solo lectura", `value="app_tienda|%"`, "dbmasteruser</code> (administrador)", "data-confirm="},
			"db_table": {"Ana &lt;b&gt;", "binario · 2 KB", "NULL", "Filas 51–52", "Siguientes", "Anteriores", "?fc=name&amp;fv=ana&amp;o=id", "Quitar filtro",
				`action="/databases/x/main/client_5s/users/delete"`, `name="k" value="[&#34;51&#34;]"`, "¿Eliminar este registro?", "Se elimina el registro id = 51 de la tabla users (client_5s en MainDB)",
				"en cascada, sus registros relacionados en orders", `form="del-sel"`, `name="back" value="/databases/x/main/client_5s/users?fc=name&amp;fv=ana&amp;o=id&amp;p=50"`, "db_table.js"},
			"db_table#sinclave":   {"no tiene clave primaria"},
			"db_table#estructura": {"Definición SQL", "CREATE VIEW v AS select 1", "PRIMARY", "PK"},
			"db_users": {`action="/databases/users/main/password"`, "¿Generar una contraseña nueva para app_5s?", `name="db" value="client\_5s"`,
				"¿Quitar el acceso de app_5s a client_5s?", "Solo lectura", "administrador · esta conexión", "¿Eliminar el usuario app_5s@%?", "rdsadmin",
				`<option selected>client_5s</option>`, `href="/databases/users/main"`},
			"db_user_secret":     {"Abc123xyz", `data-copy="#env"`, "world no la guarda"},
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

func TestProblemsPHPButton(t *testing.T) {
	s := &Server{pages: map[string]*template.Template{}}
	if err := s.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	d := data{"CSRF": "tok", "Problems": []problem{{Level: "err", Area: "tienda", Title: "PHP 8.3.35 → 8.3.36 disponible"}, {Level: "low", Area: "ittest", Title: "x"}},
		"PHPUpdatable": []*store.Site{{Name: "tienda"}, {Name: "mellafe"}}}
	var out strings.Builder
	if err := s.pages["monitor"].ExecuteTemplate(&out, "problems", d); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`action="/sites/php-update"`, "Actualizar PHP en 2 sitio(s)", "tienda, mellafe", "data-confirm=", "baja prioridad"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("falta %q", want)
		}
	}
}

func TestSiteFormPHPVersions(t *testing.T) {
	s := &Server{pages: map[string]*template.Template{}}
	if err := s.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	d := s.siteFormData(&store.Site{ID: 3, Kind: store.KindPHP, PHPVersion: "8.1"}, "", nil, false)
	d["CSRF"] = "tok"
	var out strings.Builder
	if err := s.pages["site_form"].ExecuteTemplate(&out, "layout", d); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`<option value="8.5"`, "8.5 · parches hasta dic 2029", `<option value="8.1" selected>8.1 · sin parches desde dic 2025`, `name="deploy_mode"`,
		`name="redirect_mode" value="none" checked`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("falta %q", want)
		}
	}
}

func TestSiteRedirectSome(t *testing.T) {
	s := &Server{pages: map[string]*template.Template{}}
	if err := s.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	site := &store.Site{ID: 3, Name: "statsup", Kind: store.KindPHP, PHPVersion: "8.5",
		Domains: []string{"statsup.cl", "home.statsup.cl", "www.statsup.cl"}, RedirectDomains: []string{"www.statsup.cl"}}
	d := s.siteFormData(site, "", nil, false)
	d["CSRF"] = "tok"
	var out strings.Builder
	if err := s.pages["site_form"].ExecuteTemplate(&out, "layout", d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `name="redirect_mode" value="some" checked`) || !strings.Contains(out.String(), ">www.statsup.cl</textarea>") {
		t.Error("formulario: falta la opción «solo estos» marcada con el www")
	}
	out.Reset()
	if err := s.pages["site"].ExecuteTemplate(&out, "layout", data{"Site": site, "CSRF": "tok"}); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out.String(), "→ redirige al principal"); n != 1 {
		t.Errorf("página del sitio: %d dominios marcados como redirección, quería 1", n)
	}
}

func TestSQLPageRenders(t *testing.T) {
	s := &Server{pages: map[string]*template.Template{}}
	if err := s.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	stmts, _ := dbmigrate.SplitScript("SELECT 1;\nUPDATE t SET a = 1;\nDROP TABLE x;")
	plan := dbmigrate.Plan(stmts)
	inst := &dbInstanceView{Key: "main", Name: "MainDB", User: "dbmasteruser"}
	run := &dbmigrate.ScriptRun{RolledBack: true, Skipped: 1, Results: []dbmigrate.StatementResult{
		{Statement: stmts[0], HasRows: true, Columns: []string{"1"}, Rows: [][]dbmigrate.Cell{{{Text: "1"}}}, Total: 1, Sets: 1},
		{Statement: stmts[1], Err: "Error 1146 (42S02): Table 'tienda.t' doesn't exist"}}}
	for name, d := range map[string]data{
		"editor": {"Inst": inst, "Tab": "sql", "DBs": []string{"tienda"}, "SelDB": "tienda", "Tx": true, "RowsLimit": 500},
		"review": {"Inst": inst, "Tab": "sql", "SelDB": "tienda", "Tx": true, "Plan": plan, "Review": true, "Script": "SELECT 1;\nUPDATE t SET a = 1;\nDROP TABLE x;"},
		"run":    {"Inst": inst, "Tab": "sql", "SelDB": "tienda", "Tx": true, "Plan": plan, "Run": run},
	} {
		d["CSRF"] = "tok"
		var out strings.Builder
		if err := s.pages["db_sql"].ExecuteTemplate(&out, "layout", d); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		html := out.String()
		want := map[string][]string{
			"editor": {`enctype="multipart/form-data"`, `<option selected>tienda</option>`, `name="tx" value="1" checked`, `class="active" aria-current="page"`},
			"review": {"Revisa antes de ejecutar", "1 UPDATE, 1 DROP TABLE", "línea 3: DROP TABLE", `name="confirmed" value="1"`, "data-confirm=", "UPDATE t SET a = 1;"},
			"run":    {"Se detuvo en la sentencia de la línea 2", "Se deshizo la transacción", "doesn&#39;t exist", "1 sin ejecutar", "<th>1</th>"},
		}[name]
		for _, w := range want {
			if !strings.Contains(html, w) {
				t.Errorf("%s: falta %q", name, w)
			}
		}
	}
}
