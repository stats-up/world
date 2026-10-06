//go:build integration

// Prueba de la consola web contra el Docker real: levanta un contenedor desechable,
// abre la shell por WebSocket como lo haría el navegador y ejecuta comandos.
//
//	go test -c -tags integration -o web-it ./internal/web
//	sudo ./web-it -test.v -test.run Console
package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"world/internal/auth"
	"world/internal/config"
	"world/internal/deploy"
	"world/internal/docker"
	"world/internal/mail"
	"world/internal/monitor"
	"world/internal/proxy"
	"world/internal/secret"
	"world/internal/store"
)

func TestIntegrationConsole(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := config.Config{DataDir: dir, DockerSocket: "/var/run/docker.sock"}
	st, err := store.Open(filepath.Join(dir, "it.db"))
	if err != nil {
		t.Fatal(err)
	}
	box, _ := secret.LoadOrCreate(filepath.Join(dir, "secret.key"))
	dc := docker.New(cfg.DockerSocket)

	// Contenedor desechable con la label del sitio, como los que crea un deploy.
	const name = "consoletest"
	if err := dc.ImagePull(ctx, "alpine:3"); err != nil {
		t.Fatal(err)
	}
	cid, err := dc.ContainerCreate(ctx, "world-"+name+"-it", docker.ContainerConfig{
		Image: "alpine:3", Cmd: []string{"sleep", "600"}, Labels: map[string]string{"world.site": name, "world.deployment": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dc.ContainerRemove(context.Background(), cid) })
	if err := dc.ContainerStart(ctx, cid); err != nil {
		t.Fatal(err)
	}
	siteID, err := st.CreateSite(&store.Site{Name: name, RepoURL: "https://example.com/x.git", Branch: "main", Kind: store.KindStatic})
	if err != nil {
		t.Fatal(err)
	}

	// Usuario y sesión.
	uid, _ := st.CreateUser("it@example.test", "x")
	token, csrf := auth.RandomToken(24), auth.RandomToken(24)
	st.CreateSession(auth.HashToken(token), uid, csrf, false, time.Now().Add(time.Hour))

	engine := deploy.New(cfg, st, dc, box)
	srv, err := New(cfg, st, dc, box, engine, deploy.NewPoller(engine, st, time.Minute),
		deploy.NewCron(engine, st, time.Minute), proxy.New(dc, cfg), mail.New(dc, cfg), monitor.New(dc, st), "it")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	cookie := &http.Cookie{Name: sessionCookie, Value: token}
	base := ts.URL + "/sites/" + strconv.FormatInt(siteID, 10) + "/console"

	// La página carga con la CSP que necesita xterm.js.
	req, _ := http.NewRequest("GET", base, nil)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("página de consola: %v %v", err, resp.Status)
	}
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "'unsafe-inline'") {
		t.Error("la página de consola no relaja style-src para xterm.js")
	}
	resp.Body.Close()

	dial := func(query string, origin string) (*websocket.Conn, *http.Response, error) {
		h := http.Header{}
		h.Set("Cookie", cookie.String())
		h.Set("Origin", origin)
		return websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/ws"+query, &websocket.DialOptions{HTTPHeader: h})
	}

	// Sin CSRF o desde otro origen: rechazado.
	if _, r, err := dial("", ts.URL); err == nil || r.StatusCode != http.StatusForbidden {
		t.Errorf("sin csrf debió responder 403")
	}
	if _, r, err := dial("?csrf="+csrf, "https://evil.example"); err == nil || r.StatusCode != http.StatusForbidden {
		t.Errorf("desde otro origen debió responder 403")
	}

	for _, user := range []string{"", "root"} {
		q := "?csrf=" + csrf
		if user != "" {
			q += "&user=" + user
		}
		ws, _, err := dial(q, ts.URL)
		if err != nil {
			t.Fatalf("websocket (%q): %v", user, err)
		}
		ws.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":90,"rows":20}`))
		ws.Write(ctx, websocket.MessageBinary, []byte("echo hola-$((40+2)) $(id -un) $(stty size)\r"))
		out := readUntil(t, ws, "hola-42")
		t.Logf("usuario %q → %q", user, lastLine(out, "hola-42"))
		if !strings.Contains(out, "20 90") {
			t.Errorf("el resize no se aplicó al TTY: %q", out)
		}
		ws.Write(ctx, websocket.MessageBinary, []byte("exit\r"))
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		for {
			if _, _, err := ws.Read(rctx); err != nil {
				if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
					t.Errorf("cierre inesperado: %v", err)
				}
				break
			}
		}
		cancel()
	}
}

func readUntil(t *testing.T, ws *websocket.Conn, want string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var b strings.Builder
	for {
		_, msg, err := ws.Read(ctx)
		if err != nil {
			t.Fatalf("no llegó %q: %v\nsalida: %q", want, err, b.String())
		}
		b.Write(msg)
		// El eco del comando trae "$((40+2))" sin calcular: "hola-42 " solo aparece en la salida.
		if strings.Contains(b.String(), want+" ") {
			return b.String()
		}
	}
}

func lastLine(s, want string) string {
	i := strings.LastIndex(s, want)
	line := s[i:]
	if j := strings.IndexAny(line, "\r\n"); j >= 0 {
		line = line[:j]
	}
	return line
}
