package web

import (
	"html/template"
	"strings"
	"testing"

	"world/internal/mail"
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
	pages := map[string]data{
		"mail":                {"Domains": []mail.Domain{*dom}, "Hostname": "mx.statsup.cl"},
		"mail_domain":         {"Domain": dom, "Accounts": []mail.Account{acc}, "Tab": "ajustes", "Hostname": "mx.statsup.cl"},
		"mail_domain#buzones": {"Domain": dom, "Accounts": []mail.Account{acc}, "Tab": "buzones", "Hostname": "mx.statsup.cl"},
		"mail_domain#dns":     {"Domain": dom, "Accounts": []mail.Account(nil), "Records": recs, "Pending": 1, "Tab": "dns", "Hostname": "mx.statsup.cl"},
		"mail_domain_new":     {"Hostname": "mx.statsup.cl", "Form": map[string]string{"mode": "migration"}, "Error": "Dominio inválido."},
		"mail_account_new":    {"Domain": dom, "Form": map[string]string{"quota_gb": "5"}},
		"mail_account":        {"Domain": dom, "Account": &acc, "QuotaGB": "5", "Aliases": "info", "Hostname": "mx.statsup.cl"},
		"mail_password":       {"Domain": dom, "Email": acc.Email, "Password": "Secreto123", "Created": true, "Hostname": "mx.statsup.cl"},
	}
	for name, d := range pages {
		d["CSRF"] = "tok"
		var b strings.Builder
		page, _, _ := strings.Cut(name, "#")
		if err := s.pages[page].ExecuteTemplate(&b, "layout", d); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for want, in := range map[string]string{
			`href="/mail/domains/c/accounts/a"`:   "mail_domain#buzones",
			`data-filter="#accounts"`:             "mail_domain#buzones",
			"10 mx.statsup.cl":                    "mail_domain#dns",
			`action="/mail/domains"`:              "mail_domain_new",
			`action="/mail/domains/c/accounts"`:   "mail_account_new",
			`href="/mail/domains/c/accounts/new"`: "mail_domain",
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
