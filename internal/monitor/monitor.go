// Package monitor toma muestras de CPU, RAM, disco y red del servidor y de cada sitio,
// y las guarda en SQLite (detalle por minuto 48 h, resumen cada 15 min por 30 días).
package monitor

import (
	"context"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"world/internal/docker"
	"world/internal/store"
)

const (
	labelSite    = "world.site"
	labelManaged = "world.managed"
)

// ServiceKey es la clave de un servicio de world (ej: "@traefik") para distinguirlo de un sitio.
func ServiceKey(name string) string { return "@" + name }

// Snapshot es la última muestra completa.
type Snapshot struct {
	At   time.Time
	Host store.HostPoint
	Ctr  map[string]store.CtrPoint // por sitio o servicio
}

// Services devuelve las muestras de los servicios de world (Traefik, Stalwart) ordenadas.
func (s *Snapshot) Services() []store.CtrPoint {
	var out []store.CtrPoint
	for k, p := range s.Ctr {
		if len(k) > 0 && k[0] == '@' {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

type ctrPrev struct {
	read           time.Time
	cpu, system    uint64
	rx, tx, br, bw uint64
}

type Monitor struct {
	dc       *docker.Client
	st       *store.Store
	Interval time.Duration
	Proc     string // raíz de /proc del servidor
	DiskPath string // sistema de archivos a medir
	Cgroup   string // raíz de cgroup v2 del servidor

	mu       sync.Mutex
	latest   *Snapshot
	lastErr  string
	prevHost *hostRaw
	prevCtr  map[string]ctrPrev // por id de contenedor
	ncpu     int
}

func New(dc *docker.Client, st *store.Store) *Monitor {
	return &Monitor{dc: dc, st: st, Interval: 30 * time.Second, Proc: "/proc", DiskPath: "/", Cgroup: "/sys/fs/cgroup", prevCtr: map[string]ctrPrev{}}
}

// Latest devuelve la última muestra (nil si aún no hay) y el último error.
func (m *Monitor) Latest() (*Snapshot, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.latest, m.lastErr
}

// NCPU es la cantidad de CPU del servidor (0 si aún no se sabe).
func (m *Monitor) NCPU() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ncpu
}

func (m *Monitor) Run(ctx context.Context) {
	lastRollup := time.Now().Unix() / store.ResQuarter * store.ResQuarter
	t := time.NewTicker(m.Interval)
	defer t.Stop()
	m.sample(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			m.sample(ctx)
			// Al cerrar cada bloque de 15 min se resume (se repasa también el anterior por si el panel estuvo detenido).
			if q := now.Unix() / store.ResQuarter * store.ResQuarter; q > lastRollup {
				if err := m.st.RollupMetrics(lastRollup-store.ResQuarter, q, now); err != nil {
					log.Printf("monitor: resumen: %v", err)
				}
				lastRollup = q
			}
		}
	}
}

func (m *Monitor) sample(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	now := time.Now()
	snap := &Snapshot{At: now, Ctr: map[string]store.CtrPoint{}}
	var errs []string

	if raw, err := readHost(m.Proc, m.DiskPath); err != nil {
		errs = append(errs, "servidor: "+err.Error())
	} else {
		snap.Host = store.HostPoint{TS: now.Unix(), MemUsed: raw.memUsed, MemTotal: raw.memTotal,
			SwapUsed: raw.swapUsed, SwapTotal: raw.swapTotal, DiskUsed: raw.diskUsed, DiskTotal: raw.diskTotal, Load1: raw.load1}
		if p := m.prevHost; p != nil && raw.cpu.total > p.cpu.total {
			snap.Host.CPU = 100 * float64(raw.cpu.busy-p.cpu.busy) / float64(raw.cpu.total-p.cpu.total)
		}
		first := m.prevHost == nil
		m.prevHost = &raw
		if !first {
			if err := m.st.SaveHostMetric(snap.Host); err != nil {
				errs = append(errs, "guardando: "+err.Error())
			}
		}
	}

	if err := m.sampleContainers(ctx, snap); err != nil {
		errs = append(errs, "docker: "+err.Error())
	}
	var points []store.CtrPoint
	for _, p := range snap.Ctr {
		points = append(points, p)
	}
	if len(points) > 0 {
		if err := m.st.SaveCtrMetrics(points); err != nil {
			errs = append(errs, "guardando: "+err.Error())
		}
	}

	m.mu.Lock()
	m.latest = snap
	m.lastErr = ""
	if len(errs) > 0 {
		m.lastErr = errs[0]
		log.Printf("monitor: %v", errs)
	}
	m.mu.Unlock()
}

// sampleContainers suma por sitio/servicio los contenedores en marcha (durante un deploy conviven dos).
func (m *Monitor) sampleContainers(ctx context.Context, snap *Snapshot) error {
	var list []docker.ContainerSummary
	for _, label := range []string{labelSite, labelManaged} {
		l, err := m.dc.ContainersByLabel(ctx, label)
		if err != nil {
			return err
		}
		list = append(list, l...)
	}
	seen := map[string]bool{}
	for _, c := range list {
		if c.State != "running" {
			continue
		}
		key := c.Labels[labelSite]
		if key == "" {
			key = ServiceKey(c.Labels[labelManaged])
		}
		st, err := m.dc.ContainerStats(ctx, c.ID)
		if err != nil {
			continue // el contenedor pudo terminar entre el listado y la lectura
		}
		seen[c.ID] = true
		var rx, tx, br, bw uint64
		for _, n := range st.Networks {
			rx += n.RxBytes
			tx += n.TxBytes
		}
		for _, b := range st.BlkioStats.IOServiceBytesRecursive {
			switch b.Op {
			case "read", "Read":
				br += b.Value
			case "write", "Write":
				bw += b.Value
			}
		}
		cur := ctrPrev{read: st.Read, cpu: st.CPUStats.CPUUsage.TotalUsage, system: st.CPUStats.SystemUsage, rx: rx, tx: tx, br: br, bw: bw}
		if st.CPUStats.OnlineCPUs > 0 {
			m.mu.Lock()
			m.ncpu = st.CPUStats.OnlineCPUs
			m.mu.Unlock()
		}

		p := snap.Ctr[key]
		p.Key, p.TS = key, snap.At.Unix()
		p.Mem += int64(st.MemUsed())
		if lim := int64(st.MemoryStats.Limit); lim > p.MemLimit {
			p.MemLimit = lim
		}
		if prev, ok := m.prevCtr[c.ID]; ok {
			if cur.system > prev.system && cur.cpu >= prev.cpu {
				p.CPU += float64(cur.cpu-prev.cpu) / float64(cur.system-prev.system) * float64(st.CPUStats.OnlineCPUs)
			}
			if dt := cur.read.Sub(prev.read).Seconds(); dt > 0 {
				p.NetRx += rate(cur.rx, prev.rx, dt)
				p.NetTx += rate(cur.tx, prev.tx, dt)
				p.BlkR += rate(cur.br, prev.br, dt)
				p.BlkW += rate(cur.bw, prev.bw, dt)
			}
		}
		m.prevCtr[c.ID] = cur
		if info, err := m.dc.ContainerInspect(ctx, c.ID); err == nil {
			p.Restarts += int64(info.RestartCount)
			if info.State.OOMKilled {
				p.OOMKills++
			}
		}
		p.OOMKills += m.oomKills(c.ID)
		snap.Ctr[key] = p
	}
	for id := range m.prevCtr {
		if !seen[id] {
			delete(m.prevCtr, id)
		}
	}
	return nil
}

func rate(cur, prev uint64, dt float64) float64 {
	if cur < prev { // el contador se reinició
		return 0
	}
	return float64(cur-prev) / dt
}

// oomKills lee del cgroup del contenedor cuántos procesos mató el kernel por falta de memoria.
// Docker solo lo informa si muere el proceso principal; con PHP-FPM normalmente muere un worker y nada más.
func (m *Monitor) oomKills(id string) int64 {
	for _, dir := range []string{"/system.slice/docker-" + id + ".scope", "/docker/" + id} {
		data, err := os.ReadFile(m.Cgroup + dir + "/memory.events")
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if v, ok := strings.CutPrefix(line, "oom_kill "); ok {
				n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
				return n
			}
		}
	}
	return 0
}
