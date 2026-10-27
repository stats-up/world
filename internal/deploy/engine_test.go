package deploy

import (
	"testing"

	"world/internal/store"
)

func TestVolumeName(t *testing.T) {
	cases := map[string]string{
		"/var/www/html/storage": "world-tienda-var-www-html-storage",
		"/data":                 "world-tienda-data",
		"/srv/app/.cache":       "world-tienda-srv-app-.cache",
	}
	for path, want := range cases {
		if got := VolumeName("tienda", path); got != want {
			t.Errorf("VolumeName(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestTraefikLabelsRedirect(t *testing.T) {
	site := &store.Site{Name: "ff", Kind: store.KindPHP, RedirectAliases: true,
		Domains: []string{"frikiforja.cl", "www.frikiforja.cl", "tienda.frikiforja.cl"}}
	l := TraefikLabels(site)
	if got := l["traefik.http.routers.site-ff.rule"]; got != "Host(`frikiforja.cl`)" {
		t.Errorf("router principal = %q", got)
	}
	if got := l["traefik.http.routers.site-ff-alias.rule"]; got != "Host(`www.frikiforja.cl`) || Host(`tienda.frikiforja.cl`)" {
		t.Errorf("router alias = %q", got)
	}
	if got := l["traefik.http.middlewares.site-ff-redirect.redirectregex.replacement"]; got != "https://frikiforja.cl${1}" {
		t.Errorf("replacement = %q", got)
	}

	site.RedirectAliases = false
	l = TraefikLabels(site)
	if _, ok := l["traefik.http.routers.site-ff-alias.rule"]; ok {
		t.Error("sin RedirectAliases no debe haber router de alias")
	}
	if got := l["traefik.http.routers.site-ff.rule"]; got != "Host(`frikiforja.cl`) || Host(`www.frikiforja.cl`) || Host(`tienda.frikiforja.cl`)" {
		t.Errorf("router sin redirección = %q", got)
	}

	// Solo el www redirige; tienda sigue llegando a la app. Un dominio que ya no está en la lista no cuenta.
	site.RedirectDomains = []string{"www.frikiforja.cl", "viejo.frikiforja.cl"}
	l = TraefikLabels(site)
	if got := l["traefik.http.routers.site-ff.rule"]; got != "Host(`frikiforja.cl`) || Host(`tienda.frikiforja.cl`)" {
		t.Errorf("router con www redirigido = %q", got)
	}
	if got := l["traefik.http.routers.site-ff-alias.rule"]; got != "Host(`www.frikiforja.cl`)" {
		t.Errorf("router alias solo www = %q", got)
	}
	site.RedirectDomains = nil

	site.RedirectAliases = true
	site.Domains = site.Domains[:1]
	if _, ok := TraefikLabels(site)["traefik.http.routers.site-ff-alias.rule"]; ok {
		t.Error("con un solo dominio no debe haber router de alias")
	}
}
