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
