package web

import (
	"html/template"
	"strings"
	"testing"
	"time"

	"world/internal/mail"
)

func TestMailSecurityPage(t *testing.T) {
	s := &Server{pages: map[string]*template.Template{}}
	if err := s.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	server := []serverAddr{{Address: "54.20.26.50", Label: "IP pública del servidor (mx.statsup.cl)"},
		{Address: "172.18.0.0/16", Label: "Red interna de Docker", Allowed: true}}
	auth, scan := "authFailure", "portScanning"
	ip1 := mail.IPEntry{ID: "b1", Address: "54.20.26.50", Reason: &auth, CreatedAt: time.Now().Add(-time.Hour)}
	ip2 := mail.IPEntry{ID: "b2", Address: "165.154.11.140", Reason: &scan, CreatedAt: time.Now()}
	rows := []blockedRow{{IPEntry: ip1, Server: serverLabel(ip1.Address, server)}, {IPEntry: ip2, Server: serverLabel(ip2.Address, server)}}
	if rows[0].Server == "" || rows[1].Server != "" || serverLabel("172.18.0.9", server) == "" {
		t.Fatalf("serverLabel: %+v", rows)
	}
	reason := "red docker"
	d := data{"Blocked": rows, "ServerBlocked": rows[:1], "ServerAddrs": server, "CSRF": "tok",
		"Allowed": []mail.IPEntry{{ID: "a1", Address: "172.18.0.0/16", Reason: &reason, CreatedAt: time.Now()}}}
	var out strings.Builder
	if err := s.pages["mail_security"].ExecuteTemplate(&out, "layout", d); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{"Stalwart está bloqueando una dirección del propio servidor", `name="id" value="b1"`,
		"Fallas de autenticación", "Escaneo de puertos", `data-reason="portScanning"`, `action="/mail/security/allowed/a1/delete"`,
		"data-confirm=", `name="address" value="54.20.26.50"`, "permitida", `href="/mail/security"`, `aria-current="page"`, "nunca"} {
		if !strings.Contains(html, want) {
			t.Errorf("falta %q", want)
		}
	}
	// La pestaña Dominios en la página de correo.
	out.Reset()
	if err := s.pages["mail"].ExecuteTemplate(&out, "layout", data{"CSRF": "tok"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `<a href="/mail/security">`) {
		t.Error("mail: falta la pestaña Seguridad")
	}
}
