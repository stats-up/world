//go:build integration

// Prueba de integración del deploy "recreate" contra el Docker real del servidor:
//
//	go test -c -tags integration -o deploy-it ./internal/deploy
//	sudo WORLD_IT_RECREATE=1 ./deploy-it -test.run Recreate -test.v
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

func TestIntegrationRecreate(t *testing.T) {
	if os.Getenv("WORLD_IT_RECREATE") == "" {
		t.Skip("define WORLD_IT_RECREATE=1 para correr esta prueba")
	}
	dir := t.TempDir()
	// Cada contenedor anota en /data/ev cuándo arranca y cuándo termina de cerrar (tarda 3 s, como
	// Chromium cerrando sesiones). Si el nuevo arrancara antes de que el anterior cierre, se nota.
	repo := filepath.Join(dir, "repo")
	os.MkdirAll(repo, 0o755)
	os.WriteFile(filepath.Join(repo, "Dockerfile"), []byte("FROM "+copyImage+"\n"+
		`CMD ["sh", "-c", "[ -n \"$FAIL\" ] && exit 1; echo start $HOSTNAME >> /data/ev; trap 'sleep 3; echo stopped $HOSTNAME >> /data/ev; exit 0' TERM; while true; do sleep 1 & wait $!; done"]`+"\n"), 0o644)
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
	const name, data = "rntest-recreate", "/data"
	site := &store.Site{Name: name, RepoURL: repo, Branch: "main", Kind: store.KindDockerfile, Port: 8080,
		EnvEnc: box.SealString(""), MemoryMB: 64, CPUs: 0.5, PersistPaths: []string{data},
		DeployMode: store.DeployRecreate, StopTimeout: 10}
	id, err := st.CreateSite(site)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dc := docker.New(cfg.DockerSocket)
	e := New(cfg, st, dc, box)
	cron := NewCron(e, st, time.Minute)
	t.Cleanup(func() {
		e.Remove(ctx, &store.Site{Name: name})
		dc.VolumeRemove(ctx, VolumeName(name, data))
	})
	running := func() []docker.ContainerSummary {
		cs, _ := dc.ContainersByLabel(ctx, labelSite+"="+name)
		var out []docker.ContainerSummary
		for _, c := range cs {
			if c.State == "running" {
				out = append(out, c)
			}
		}
		return out
	}
	only := func() docker.ContainerSummary {
		t.Helper()
		cs := running()
		if len(cs) != 1 {
			t.Fatalf("se esperaba 1 contenedor corriendo: %+v", cs)
		}
		return cs[0]
	}
	events := func(id string) string {
		out, _, _ := dc.ContainerExec(ctx, id, []string{"cat", "/data/ev"})
		return strings.TrimSpace(out)
	}
	log := func(depID int64) string { b, _ := os.ReadFile(e.LogPath(depID)); return string(b) }
	if d, _ := st.Deployment(runDeploy(t, e, st, id)); d.Status != store.DeploySuccess {
		t.Fatal(d.Error)
	}
	first := only()

	// 1. Deploy con el cron en curso: espera que termine, detiene el anterior (que cierra en 3 s)
	//    y recién entonces arranca el nuevo. Nunca hay dos corriendo.
	cron.mu.Lock()
	cron.running[id] = true
	cron.mu.Unlock()
	go func() {
		time.Sleep(4 * time.Second)
		cron.mu.Lock()
		delete(cron.running, id)
		cron.mu.Unlock()
	}()
	both := make(chan bool, 1)
	stop := make(chan struct{})
	go func() {
		seen := false
		for {
			select {
			case <-stop:
				both <- seen
				return
			case <-time.After(200 * time.Millisecond):
				if len(running()) > 1 {
					seen = true
				}
			}
		}
	}()
	dep := runDeploy(t, e, st, id)
	close(stop)
	if d, _ := st.Deployment(dep); d.Status != store.DeploySuccess {
		t.Fatalf("deploy falló: %s\n%s", d.Error, log(dep))
	}
	if <-both {
		t.Fatalf("corrieron dos contenedores a la vez\n%s", log(dep))
	}
	l := log(dep)
	t.Logf("log:\n%s", l)
	if !strings.Contains(l, "El cron terminó") || !strings.Contains(l, "Deteniendo la versión actual") {
		t.Fatalf("faltan pasos en el log:\n%s", l)
	}
	if cron.held[id] {
		t.Fatal("el cron debió reanudarse")
	}
	second := only()
	ev := events(second.ID)
	lines := strings.Split(ev, "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[1], "stopped ") || !strings.HasPrefix(lines[2], "start ") {
		t.Fatalf("orden de eventos: el anterior debe cerrar antes de que arranque el nuevo:\n%s", ev)
	}
	if second.ID == first.ID {
		t.Fatal("debió crearse un contenedor nuevo")
	}

	// 2. Si el nuevo no arranca, vuelve a levantarse el anterior.
	s, _ := st.Site(id)
	s.EnvEnc = box.SealString("FAIL=1")
	st.UpdateSite(s)
	dep, err = e.Start(id, store.SourceManual)
	if err != nil {
		t.Fatal(err)
	}
	var d *store.Deployment
	for i := 0; i < 300; i++ {
		if d, _ = st.Deployment(dep); !d.Running() {
			break
		}
		time.Sleep(time.Second)
	}
	if d.Status != store.DeployFailed {
		t.Fatalf("debió fallar:\n%s", log(dep))
	}
	if now := only(); now.ID != second.ID {
		t.Fatalf("debió volver el mismo contenedor: %s ≠ %s\n%s", now.ID, second.ID, log(dep))
	}
	if !strings.Contains(log(dep), "Volviendo a levantar la versión anterior") {
		t.Fatalf("falta el aviso de vuelta atrás:\n%s", log(dep))
	}
}
