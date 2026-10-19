package web

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSourceGuard(t *testing.T) {
	traefik := []net.IP{net.ParseIP("172.18.0.2")}
	lookups := 0
	tp := &trustedProxy{lookup: func(context.Context) ([]net.IP, error) { lookups++; return traefik, nil }}
	h := sourceGuard(tp, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(clientIP(r))) }))

	cases := []struct {
		name, remote, xff string
		code              int
		ip                string
	}{
		{"traefik con XFF", "[::ffff:172.18.0.2]:34438", "1.2.3.4, 5.6.7.8", 200, "5.6.7.8"},
		{"traefik sin XFF", "172.18.0.2:1000", "", 200, "172.18.0.2"},
		{"traefik con XFF inválido", "172.18.0.2:1000", "basura", 200, "172.18.0.2"},
		{"otro contenedor", "172.18.0.7:1000", "9.9.9.9", 403, ""},
		{"red docker0", "172.17.0.5:1000", "", 403, ""},
		{"vpc lightsail", "172.26.3.10:1000", "", 403, ""},
		{"local ignora XFF", "127.0.0.1:1000", "9.9.9.9", 200, "127.0.0.1"},
		{"pública ignora XFF", "200.1.2.3:1000", "9.9.9.9", 200, "200.1.2.3"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/login", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.code {
			t.Errorf("%s: código %d, quería %d", c.name, w.Code, c.code)
			continue
		}
		if c.code == 200 && w.Body.String() != c.ip {
			t.Errorf("%s: IP %q, quería %q", c.name, w.Body.String(), c.ip)
		}
	}
	// Las IPs internas desconocidas no consultan Docker en cada petición.
	if lookups != 1 {
		t.Errorf("consultas a Docker: %d, quería 1", lookups)
	}

	// Traefik recreado con otra IP: se detecta al volver a consultar.
	traefik = []net.IP{net.ParseIP("172.18.0.9")}
	tp.checked = tp.checked.Add(-proxyRecheck)
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "172.18.0.9:1"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Errorf("Traefik con IP nueva: código %d", w.Code)
	}
}
