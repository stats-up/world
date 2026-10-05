//go:build integration

// Prueba de integración del ciclo completo de deploy contra el Docker real del servidor.
// Requiere root, Docker, git y que el panel ya haya levantado Traefik y la red "world".
//
//	go test -c -tags integration -o deploy-it ./internal/deploy
//	sudo WORLD_IT_DOMAIN=ittest.1-2-3-4.sslip.io WORLD_IT_BRANCH=12.x ./deploy-it -test.v -test.timeout 30m
//
// También prueba: carpeta persistente entre deploys, cron dentro del contenedor y redirección de alias.
package deploy

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"world/internal/config"
	"world/internal/docker"
	"world/internal/secret"
	"world/internal/store"
)

func TestIntegrationDeployLaravel(t *testing.T) {
	domain := os.Getenv("WORLD_IT_DOMAIN")
	if domain == "" {
		t.Skip("define WORLD_IT_DOMAIN para correr esta prueba")
	}
	repo := envOr("WORLD_IT_REPO", "https://github.com/laravel/laravel.git")
	branch := envOr("WORLD_IT_BRANCH", "main")

	dir := t.TempDir()
	cfg := config.Config{DataDir: dir, DockerSocket: "/var/run/docker.sock"}
	st, err := store.Open(filepath.Join(dir, "it.db"))
	if err != nil {
		t.Fatal(err)
	}
	box, err := secret.LoadOrCreate(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	rand.Read(key)
	env := strings.Join([]string{
		"APP_KEY=base64:" + base64.StdEncoding.EncodeToString(key),
		"APP_ENV=production",
		"APP_DEBUG=false",
		"APP_URL=https://" + domain,
		"SESSION_DRIVER=file",
		"CACHE_STORE=file",
	}, "\n")
	const storage = "/var/www/html/storage"
	alias := "www." + domain
	site := &store.Site{Name: itName, RepoURL: repo, Branch: branch, Kind: store.KindPHP, PHPVersion: "8.4",
		BuildAssets: true, Domains: []string{domain, alias}, RedirectAliases: true, EnvEnc: box.SealString(env),
		MemoryMB: 384, CPUs: 1, PersistPaths: []string{storage}, CronCommand: "php artisan schedule:run"}
	id, err := st.CreateSite(site)
	if err != nil {
		t.Fatal(err)
	}
	site.ID = id

	dc := docker.New(cfg.DockerSocket)
	e := New(cfg, st, dc, box)
	t.Cleanup(func() {
		if os.Getenv("WORLD_IT_KEEP") == "" {
			e.Remove(context.Background(), site)
			dc.VolumeRemove(context.Background(), VolumeName(itName, storage))
		}
	})

	// Dos deploys seguidos: el segundo prueba el reemplazo sin cortes y el retiro del contenedor anterior.
	for round := 1; round <= 2; round++ {
		start := time.Now()
		depID := runDeploy(t, e, st, id)
		t.Logf("deploy %d (#%d) OK en %s", round, depID, time.Since(start).Round(time.Second))

		cs, err := dc.ContainersByLabel(context.Background(), labelSite+"="+itName)
		if err != nil {
			t.Fatal(err)
		}
		if len(cs) != 1 {
			t.Fatalf("se esperaba 1 contenedor tras el deploy, hay %d", len(cs))
		}
		body := fetchThroughTraefik(t, domain)
		if !strings.Contains(body, "Laravel") {
			t.Fatalf("la respuesta no parece Laravel:\n%.500s", body)
		}
		t.Logf("Traefik → contenedor OK (%d bytes)", len(body))

		ctx := context.Background()
		const marker = storage + "/app/world-it-persist.txt"
		if round == 1 {
			if out, code, err := dc.ContainerExec(ctx, cs[0].ID, []string{"sh", "-c", "echo persiste > " + marker}); err != nil || code != 0 {
				t.Fatalf("no se pudo escribir en storage: code=%d err=%v %s", code, err, out)
			}
		} else {
			out, code, err := dc.ContainerExec(ctx, cs[0].ID, []string{"cat", marker})
			if err != nil || code != 0 || !strings.Contains(out, "persiste") {
				t.Fatalf("el archivo de storage no sobrevivió al deploy: code=%d err=%v %q", code, err, out)
			}
			t.Log("carpeta persistente OK: el archivo sobrevivió al segundo deploy")
		}
	}

	// Cron: schedule:run dentro del contenedor vigente.
	site, _ = st.Site(id)
	NewCron(e, st, time.Minute).runSite(context.Background(), site)
	site, _ = st.Site(id)
	if site.LastCronAt.IsZero() || site.LastCronExit != 0 {
		t.Fatalf("cron falló: exit=%d salida=%q", site.LastCronExit, site.LastCronOutput)
	}
	t.Logf("cron OK en %d ms: %q", site.LastCronMS, strings.TrimSpace(site.LastCronOutput))

	// Alias: debe responder 301 hacia el dominio principal conservando la ruta.
	code, loc := headThroughTraefik(t, alias, "/precios?x=1")
	if code != http.StatusMovedPermanently || loc != "https://"+domain+"/precios?x=1" {
		t.Fatalf("redirección del alias: HTTP %d Location=%q", code, loc)
	}
	t.Logf("redirección OK: %s → %s", alias, loc)
}

const itName = "ittest2"

func headThroughTraefik(t *testing.T, host, path string) (int, string) {
	t.Helper()
	client := traefikClient(host)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	var last error
	for i := 0; i < 15; i++ {
		resp, err := client.Get("https://" + host + path)
		if err == nil {
			resp.Body.Close()
			return resp.StatusCode, resp.Header.Get("Location")
		}
		last = err
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("Traefik no respondió para %s: %v", host, last)
	return 0, ""
}

func traefikClient(host string) *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, ServerName: host},
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, "127.0.0.1:443")
			},
		},
	}
}

func runDeploy(t *testing.T, e *Engine, st *store.Store, siteID int64) int64 {
	t.Helper()
	depID, err := e.Start(siteID, store.SourceManual)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Minute)
	for time.Now().Before(deadline) {
		d, err := st.Deployment(depID)
		if err != nil {
			t.Fatal(err)
		}
		if !d.Running() {
			if d.Status != store.DeploySuccess {
				logs, _ := os.ReadFile(e.LogPath(depID))
				t.Fatalf("deploy falló: %s\n--- log ---\n%s", d.Error, tail(string(logs), 6000))
			}
			return depID
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatal("el deploy no terminó en 20 minutos")
	return 0
}

// fetchThroughTraefik pide la página por HTTPS a Traefik local con el SNI del dominio.
// Se ignora el certificado: Let's Encrypt puede tardar unos segundos en emitirlo.
func fetchThroughTraefik(t *testing.T, domain string) string {
	t.Helper()
	client := traefikClient(domain)
	var last error
	for i := 0; i < 15; i++ {
		resp, err := client.Get("https://" + domain + "/")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return string(b)
			}
			last = &httpStatusError{resp.StatusCode, tail(string(b), 800)}
		} else {
			last = err
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("Traefik no devolvió 200 para %s: %v", domain, last)
	return ""
}

type httpStatusError struct {
	code int
	body string
}

func (e *httpStatusError) Error() string { return "HTTP " + http.StatusText(e.code) + ": " + e.body }

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
