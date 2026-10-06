package web

import (
	"html/template"
	"strings"
	"testing"
	"time"

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
	running := &store.MailImport{ID: 7, AccountID: "a", Host: "38.18.230.11", Port: 993, SrcUser: acc.Email, Status: store.ImportRunning, Copied: 30, Total: 40, StartedAt: time.Now()}
	done := &store.MailImport{ID: 6, AccountID: "a", Host: "38.18.230.11", Port: 993, SrcUser: acc.Email, Status: store.ImportSuccess, Copied: 40, Total: 40, Bytes: 3 << 20,
		StartedAt: time.Now().Add(-time.Hour), FinishedAt: time.Now()}
	pages := map[string]data{
		"mail":                {"Domains": []mail.Domain{*dom}, "Hostname": "mx.statsup.cl"},
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
			"30 de 40 correos revisados":                          "mail_account#import",
			`action="/mail/domains/c/accounts/a/import"`:          "mail_account#synced",
			"Sincronizar de nuevo":                                "mail_account#synced",
			"40 correos copiados (3 MB)":                          "mail_account#synced",
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
