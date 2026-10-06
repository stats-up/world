//go:build integration

package monitor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"world/internal/docker"
	"world/internal/store"
)

// Corre contra el Docker y el /proc reales del servidor: go test -tags integration ./internal/monitor/
func TestIntegrationSample(t *testing.T) {
	sock := os.Getenv("WORLD_DOCKER_SOCKET")
	if sock == "" {
		sock = "/var/run/docker.sock"
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := New(docker.New(sock), st)
	ctx := context.Background()
	m.sample(ctx)
	time.Sleep(15 * time.Second)
	m.sample(ctx)
	snap, errMsg := m.Latest()
	if errMsg != "" {
		t.Fatal(errMsg)
	}
	h := snap.Host
	t.Logf("servidor: cpu=%.1f%% ram=%d/%d MB swap=%d/%d MB disco=%d/%d GB load=%.2f ncpu=%d",
		h.CPU, h.MemUsed>>20, h.MemTotal>>20, h.SwapUsed>>20, h.SwapTotal>>20, h.DiskUsed>>30, h.DiskTotal>>30, h.Load1, m.NCPU())
	if h.MemTotal == 0 || h.DiskTotal == 0 {
		t.Fatal("faltan datos del servidor")
	}
	for k, p := range snap.Ctr {
		t.Logf("%-14s cpu=%.3f núcleos ram=%d MB límite=%d MB rx=%.0f B/s tx=%.0f B/s reinicios=%d oom=%d",
			k, p.CPU, p.Mem>>20, p.MemLimit>>20, p.NetRx, p.NetTx, p.Restarts, p.OOMKills)
	}
	if _, ok := snap.Ctr[ServiceKey("traefik")]; !ok {
		t.Error("no se midió Traefik")
	}
	s, _ := st.CtrSeries("", store.ResMinute, 0)
	h2, _ := st.HostSeries(store.ResMinute, 0)
	if len(s) == 0 || len(h2) == 0 {
		t.Fatal("no se guardaron muestras")
	}
}
