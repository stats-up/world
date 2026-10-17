//go:build integration

// Prueba de integración del renombrado contra el Docker real del servidor (sitio liviano, sin Traefik):
//
//	go test -c -tags integration -o deploy-it ./internal/deploy
//	sudo WORLD_IT_RENAME=1 ./deploy-it -test.run Rename -test.v
package deploy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"world/internal/config"
	"world/internal/docker"
	"world/internal/secret"
	"world/internal/store"
)

func TestIntegrationRename(t *testing.T) {
	if os.Getenv("WORLD_IT_RENAME") == "" {
		t.Skip("define WORLD_IT_RENAME=1 para correr esta prueba")
	}
	dir := t.TempDir()
	// Repo local con un Dockerfile mínimo: el contenedor se cae al arrancar si tiene FAIL en el ambiente.
	repo := filepath.Join(dir, "repo")
	os.MkdirAll(repo, 0o755)
	os.WriteFile(filepath.Join(repo, "Dockerfile"), []byte("FROM "+copyImage+"\n"+
		`CMD ["sh", "-c", "[ -n \"$FAIL\" ] && exit 1; while true; do sleep 1; done"]`+"\n"), 0o644)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "."},
		{"-c", "user.email=it@world", "-c", "user.name=it", "commit", "-q", "-m", "it"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}

	cfg := config.Config{DataDir: dir, DockerSocket: "/var/run/docker.sock"}
	st, err := store.Open(filepath.Join(dir, "it.db"))
	if err != nil {
		t.Fatal(err)
	}
	box, err := secret.LoadOrCreate(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	const a, b, c, data = "rntest-a", "rntest-b", "rntest-c", "/data"
	site := &store.Site{Name: a, RepoURL: repo, Branch: "main", Kind: store.KindDockerfile, Port: 8080,
		EnvEnc: box.SealString(""), MemoryMB: 64, CPUs: 0.5, PersistPaths: []string{data}}
	id, err := st.CreateSite(site)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dc := docker.New(cfg.DockerSocket)
	e := New(cfg, st, dc, box)
	t.Cleanup(func() {
		for _, n := range []string{a, b, c} {
			e.Remove(ctx, &store.Site{Name: n})
			dc.VolumeRemove(ctx, VolumeName(n, data))
		}
	})

	only := func(name string) docker.ContainerSummary {
		t.Helper()
		cs, err := dc.ContainersByLabel(ctx, labelSite+"="+name)
		if err != nil || len(cs) != 1 || cs[0].State != "running" {
			t.Fatalf("%s: se esperaba 1 contenedor corriendo: %+v %v", name, cs, err)
		}
		return cs[0]
	}
	sh := func(id, cmd string) string {
		t.Helper()
		out, code, err := dc.ContainerExec(ctx, id, []string{"sh", "-c", cmd})
		if err != nil || code != 0 {
			t.Fatalf("%s: code=%d %v %s", cmd, code, err, out)
		}
		return strings.TrimSpace(out)
	}
	wait := func(depID int64) *store.Deployment {
		t.Helper()
		for i := 0; i < 400; i++ {
			d, err := st.Deployment(depID)
			if err != nil {
				t.Fatal(err)
			}
			if !d.Running() {
				return d
			}
			time.Sleep(time.Second)
		}
		t.Fatal("el deploy no terminó")
		return nil
	}
	log := func(depID int64) string { b, _ := os.ReadFile(e.LogPath(depID)); return string(b) }

	if d := wait(runDeploy(t, e, st, id)); d.Status != store.DeploySuccess {
		t.Fatal(d.Error)
	}
	sh(only(a).ID, "echo hola > /data/x && chown 1234:1234 /data/x && chmod 640 /data/x")

	// 1. Renombrar bien: los datos pasan al volumen nuevo y no queda nada con el nombre anterior.
	dep, err := e.Rename(id, b)
	if err != nil {
		t.Fatal(err)
	}
	if d := wait(dep); d.Status != store.DeploySuccess || d.Source != store.SourceRename {
		t.Fatalf("renombrar falló: %s\n%s", d.Error, log(dep))
	}
	t.Logf("log:\n%s", log(dep))
	if s, _ := st.Site(id); s.Name != b {
		t.Fatalf("nombre en world: %s", s.Name)
	}
	if got := sh(only(b).ID, "cat /data/x; stat -c '%u:%g %a' /data/x"); got != "hola\n1234:1234 640" {
		t.Fatalf("datos copiados: %q", got)
	}
	if cs, _ := dc.ContainersByLabel(ctx, labelSite+"="+a); len(cs) != 0 {
		t.Fatalf("quedaron contenedores de %s: %+v", a, cs)
	}
	if imgs, _ := dc.ImagesByReference(ctx, "world/"+a); len(imgs) != 0 {
		t.Fatalf("quedaron imágenes de %s", a)
	}
	if ok, _ := dc.VolumeExists(ctx, VolumeName(a, data)); ok {
		t.Fatal("quedó el volumen anterior")
	}

	// 2. Si el contenedor nuevo no arranca, vuelve el actual con su nombre y sin copias.
	before := only(b).ID
	setEnv := func(env string) {
		s, _ := st.Site(id)
		s.EnvEnc = box.SealString(env)
		if err := st.UpdateSite(s); err != nil {
			t.Fatal(err)
		}
	}
	setEnv("FAIL=1") // solo lo recibe el contenedor nuevo, que se cae al iniciar
	dep, err = e.Rename(id, c)
	if err != nil {
		t.Fatal(err)
	}
	if d := wait(dep); d.Status != store.DeployFailed {
		t.Fatalf("debió fallar:\n%s", log(dep))
	}
	if s, _ := st.Site(id); s.Name != b {
		t.Fatalf("el nombre no debió cambiar: %s", s.Name)
	}
	if now := only(b).ID; now != before {
		t.Fatalf("debió volver el mismo contenedor: %s ≠ %s", now, before)
	}
	if ok, _ := dc.VolumeExists(ctx, VolumeName(c, data)); ok {
		t.Fatal("quedó la copia del intento fallido")
	}
	if cs, _ := dc.ContainersByLabel(ctx, labelSite+"="+c); len(cs) != 0 {
		t.Fatalf("quedaron contenedores de %s", c)
	}
	if got := sh(before, "cat /data/x"); got != "hola" {
		t.Fatalf("datos tras el intento fallido: %q", got)
	}
	setEnv("")

	// 3. El mismo nombre, o un volumen que ya existe con el nombre nuevo, frenan antes de detener nada.
	dep, _ = e.Rename(id, b)
	if d := wait(dep); d.Status != store.DeployFailed || !strings.Contains(d.Error, "igual") {
		t.Fatalf("mismo nombre: %s", d.Error)
	}
	tmp, err := dc.ContainerCreate(ctx, "", docker.ContainerConfig{Image: copyImage, HostConfig: docker.HostConfig{Binds: []string{VolumeName(c, data) + ":/x"}}})
	if err != nil {
		t.Fatal(err)
	}
	dc.ContainerRemove(ctx, tmp)
	dep, _ = e.Rename(id, c)
	if d := wait(dep); d.Status != store.DeployFailed || !strings.Contains(d.Error, "ya existe el volumen") {
		t.Fatalf("debió frenar por el volumen: %s", d.Error)
	}
	if only(b).ID != before {
		t.Fatal("no debió tocar el contenedor")
	}
}
