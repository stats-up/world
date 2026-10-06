package web

import (
	"fmt"
	"html/template"
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"

	"world/internal/monitor"
	"world/internal/store"
)

// --- Resumen (dashboard) ---

type monBar struct {
	Label, Text string
	Pct         float64
	Level       string // ok | warn | err
}

// Bar dibuja la barra como SVG: la CSP no permite estilos en línea (style="width:…").
func (b monBar) Bar() template.HTML {
	w := math.Max(0, math.Min(100, b.Pct))
	return template.HTML(fmt.Sprintf(`<svg class="bar" viewBox="0 0 100 6" preserveAspectRatio="none" aria-hidden="true"><rect class="track" width="100" height="6" rx="3"/><rect class="fill %s" width="%.1f" height="6" rx="3"/></svg>`, b.Level, w))
}

type ctrRow struct {
	Key, Name          string
	SiteID             int64
	CPU                float64 // % del servidor
	Mem, MemLimit      int64   // límite 0 = sin límite propio
	NetRx, NetTx       float64
	Restarts           int64
	OOMKills           int64
	CPUSpark, MemSpark template.HTML
}

func (r ctrRow) CPUText() string { return fmtPct(r.CPU) }
func (r ctrRow) MemText() string { return fmtBytes(float64(r.Mem)) }
func (r ctrRow) LimitText() string {
	if r.MemLimit == 0 {
		return ""
	}
	return fmtBytes(float64(r.MemLimit))
}
func (r ctrRow) MemPct() float64 {
	if r.MemLimit == 0 {
		return 0
	}
	return 100 * float64(r.Mem) / float64(r.MemLimit)
}
func (r ctrRow) NetText() string { return "↓ " + fmtRate(r.NetRx) + " · ↑ " + fmtRate(r.NetTx) }

type monView struct {
	Ready    bool
	At       time.Time
	Err      string
	Bars     []monBar
	Load     string
	Services []ctrRow
	Sites    map[string]*ctrRow
}

func level(pct float64) string {
	switch {
	case pct >= 90:
		return "err"
	case pct >= 75:
		return "warn"
	}
	return "ok"
}

func pct(a, b int64) float64 {
	if b <= 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}

// monitorView arma lo que muestra el dashboard; con sparks agrega los minigráficos de 24 h.
func (s *Server) monitorView(sparks bool) monView {
	snap, errMsg := s.mon.Latest()
	v := monView{Err: errMsg, Sites: map[string]*ctrRow{}}
	if snap == nil {
		return v
	}
	v.Ready, v.At = true, snap.At
	h := snap.Host
	ncpu := s.mon.NCPU()
	if h.MemTotal > 0 {
		v.Bars = append(v.Bars, monBar{Label: "CPU", Pct: h.CPU, Text: fmtPct(h.CPU), Level: level(h.CPU)})
		p := pct(h.MemUsed, h.MemTotal)
		v.Bars = append(v.Bars, monBar{Label: "RAM", Pct: p, Level: level(p),
			Text: fmtBytes(float64(h.MemUsed)) + " de " + fmtBytes(float64(h.MemTotal))})
		if h.SwapTotal > 0 {
			p := pct(h.SwapUsed, h.SwapTotal)
			v.Bars = append(v.Bars, monBar{Label: "Swap", Pct: p, Level: level(p),
				Text: fmtBytes(float64(h.SwapUsed)) + " de " + fmtBytes(float64(h.SwapTotal))})
		}
		if h.DiskTotal > 0 {
			p := pct(h.DiskUsed, h.DiskTotal)
			v.Bars = append(v.Bars, monBar{Label: "Disco", Pct: p, Level: level(p),
				Text: fmtBytes(float64(h.DiskUsed)) + " de " + fmtBytes(float64(h.DiskTotal))})
		}
		v.Load = strconv.FormatFloat(h.Load1, 'f', 2, 64)
		if ncpu > 0 {
			v.Load += " (" + strconv.Itoa(ncpu) + " vCPU)"
		}
	}

	var series map[string][]store.CtrPoint
	to := snap.At.Unix()
	from := to - 86400
	if sparks {
		series, _ = s.st.CtrSeries("", store.ResMinute, from)
	}
	ids := map[string]int64{}
	if sites, err := s.st.Sites(); err == nil {
		for _, st := range sites {
			ids[st.Name] = st.ID
		}
	}
	for key, p := range snap.Ctr {
		row := s.ctrRow(p, h.MemTotal, ncpu)
		row.SiteID = ids[key]
		if sparks {
			pts := series[key]
			row.CPUSpark = sparkline(downsample(pts, 600, func(p store.CtrPoint) float64 { return cpuPct(p.CPU, ncpu) }, false), from, to, 600, 5)
			row.MemSpark = sparkline(downsample(pts, 600, func(p store.CtrPoint) float64 { return float64(p.Mem) }, true), from, to, 600, 64<<20)
		}
		if key[0] == '@' {
			v.Services = append(v.Services, row)
		} else {
			v.Sites[key] = &row
		}
	}
	sort.Slice(v.Services, func(i, j int) bool { return v.Services[i].Key < v.Services[j].Key })
	return v
}

var serviceNames = map[string]string{monitor.ServiceKey("traefik"): "Traefik (proxy y SSL)", monitor.ServiceKey("stalwart"): "Stalwart (correo)"}

func (s *Server) ctrRow(p store.CtrPoint, hostMem int64, ncpu int) ctrRow {
	r := ctrRow{Key: p.Key, Name: p.Key, CPU: cpuPct(p.CPU, ncpu), Mem: p.Mem, NetRx: p.NetRx, NetTx: p.NetTx, Restarts: p.Restarts, OOMKills: p.OOMKills}
	if n, ok := serviceNames[p.Key]; ok {
		r.Name = n
	} else if p.Key[0] == '@' {
		r.Name = p.Key[1:]
	}
	// Sin límite propio Docker informa la RAM del servidor como límite.
	if p.MemLimit > 0 && (hostMem == 0 || p.MemLimit < hostMem*95/100) {
		r.MemLimit = p.MemLimit
	}
	return r
}

// cpuPct pasa de núcleos usados a % del servidor (100 % = todas las vCPU ocupadas).
func cpuPct(cores float64, ncpu int) float64 {
	if ncpu <= 0 {
		ncpu = 1
	}
	return 100 * cores / float64(ncpu)
}

type timed interface {
	store.CtrPoint | store.HostPoint
}

func tsOf[T timed](p T) int64 {
	switch v := any(p).(type) {
	case store.CtrPoint:
		return v.TS
	case store.HostPoint:
		return v.TS
	}
	return 0
}

// downsample agrupa los puntos en bloques de `bucket` segundos (promedio, o máximo si useMax).
func downsample[T timed](pts []T, bucket int64, val func(T) float64, useMax bool) []chartPt {
	var out []chartPt
	var cur int64 = -1
	var acc float64
	var n int
	flush := func() {
		if n == 0 {
			return
		}
		v := acc
		if !useMax {
			v = acc / float64(n)
		}
		out = append(out, chartPt{T: cur + bucket/2, V: v})
	}
	for _, p := range pts {
		if b := tsOf(p) / bucket * bucket; b != cur {
			flush()
			cur, acc, n = b, 0, 0
		}
		if useMax {
			acc = math.Max(acc, val(p))
		} else {
			acc += val(p)
		}
		n++
	}
	flush()
	return out
}

// monitorHost es el bloque del servidor que el dashboard refresca cada 30 s.
func (s *Server) monitorHost(w http.ResponseWriter, r *http.Request) {
	s.renderPartial(w, "monitor_host", s.monitorView(false))
}

// --- Gráficos del sitio ---

type siteRange struct {
	Key, Label string
	Span       time.Duration
	Res        int
	Bucket     int64
}

var siteRanges = []siteRange{
	{"24h", "24 h", 24 * time.Hour, store.ResMinute, 300},
	{"7d", "7 días", 7 * 24 * time.Hour, store.ResQuarter, 900},
	{"30d", "30 días", 30 * 24 * time.Hour, store.ResQuarter, 3600},
}

type siteMetrics struct {
	Range     siteRange
	Ranges    []siteRange
	Now       ctrRow
	HasNow    bool
	Charts    []chart
	Empty     bool
	Restarts  int64
	OOMEvents int64 // procesos terminados por falta de memoria dentro del período
}

func (s *Server) siteMetrics(site *store.Site, rangeKey string) siteMetrics {
	m := siteMetrics{Range: siteRanges[0], Ranges: siteRanges}
	for _, r := range siteRanges {
		if r.Key == rangeKey {
			m.Range = r
		}
	}
	ncpu := s.mon.NCPU()
	var hostMem int64
	if snap, _ := s.mon.Latest(); snap != nil {
		hostMem = snap.Host.MemTotal
		if p, ok := snap.Ctr[site.Name]; ok {
			m.Now, m.HasNow = s.ctrRow(p, hostMem, ncpu), true
		}
	}
	to := time.Now().Unix()
	from := to - int64(m.Range.Span.Seconds())
	series, _ := s.st.CtrSeries(site.Name, m.Range.Res, from)
	pts := series[site.Name]
	if len(pts) == 0 {
		m.Empty = true
		return m
	}
	for i, p := range pts {
		m.Restarts = max(m.Restarts, p.Restarts)
		if i > 0 && p.OOMKills > pts[i-1].OOMKills {
			m.OOMEvents += p.OOMKills - pts[i-1].OOMKills
		}
	}
	deploys, _ := s.st.DeploymentTimes(site.ID, from)
	b := m.Range.Bucket
	var cpuLimit, memLimit float64
	if site.CPUs > 0 {
		cpuLimit = cpuPct(site.CPUs, ncpu)
	}
	if site.MemoryMB > 0 {
		memLimit = float64(site.MemoryMB) * (1 << 20)
	}
	cpu := chart{Title: "CPU", From: from, To: to, Res: b, Limit: cpuLimit, MaxY: 5, Format: fmtPct, Markers: deploys,
		Series: []chartSeries{{Name: "CPU (% del servidor)", Class: "s1",
			Pts: downsample(pts, b, func(p store.CtrPoint) float64 { return cpuPct(p.CPU, ncpu) }, false)}}}
	mem := chart{Title: "RAM", From: from, To: to, Res: b, Limit: memLimit, MaxY: 64 << 20, Format: fmtBytes, Markers: deploys,
		Series: []chartSeries{{Name: "RAM usada (máximo)", Class: "s1",
			Pts: downsample(pts, b, func(p store.CtrPoint) float64 { return float64(p.Mem) }, true)}}}
	net := chart{Title: "Red", From: from, To: to, Res: b, MaxY: 1024, Format: fmtRate, Markers: deploys,
		Series: []chartSeries{
			{Name: "Entrante", Class: "s1", Pts: downsample(pts, b, func(p store.CtrPoint) float64 { return p.NetRx }, false)},
			{Name: "Saliente", Class: "s2", Pts: downsample(pts, b, func(p store.CtrPoint) float64 { return p.NetTx }, false)}}}
	m.Charts = []chart{cpu, mem, net}
	return m
}

// --- Página de monitoreo (servidor completo) ---

func pickRange(key string) siteRange {
	for _, r := range siteRanges {
		if r.Key == key {
			return r
		}
	}
	return siteRanges[0]
}

func (s *Server) monitorPage(w http.ResponseWriter, r *http.Request) {
	rg := pickRange(r.URL.Query().Get("range"))
	to := time.Now().Unix()
	from := to - int64(rg.Span.Seconds())
	b := rg.Bucket
	d := data{"Mon": s.monitorView(true), "Range": rg, "Ranges": siteRanges}
	pts, err := s.st.HostSeries(rg.Res, from)
	if err != nil {
		d["Error"] = err.Error()
	}
	if len(pts) > 0 {
		last := pts[len(pts)-1]
		d["Charts"] = []chart{
			{Title: "CPU del servidor", From: from, To: to, Res: b, MaxY: 10, Limit: 100, Format: fmtPct,
				Series: []chartSeries{{Name: "CPU", Class: "s1", Pts: downsample(pts, b, func(p store.HostPoint) float64 { return p.CPU }, false)}}},
			{Title: "RAM del servidor", From: from, To: to, Res: b, Limit: float64(last.MemTotal), Format: fmtBytes,
				Series: []chartSeries{
					{Name: "RAM usada (máximo)", Class: "s1", Pts: downsample(pts, b, func(p store.HostPoint) float64 { return float64(p.MemUsed) }, true)},
					{Name: "Swap usada", Class: "s2", Pts: downsample(pts, b, func(p store.HostPoint) float64 { return float64(p.SwapUsed) }, true)}}},
			{Title: "Disco", From: from, To: to, Res: b, Limit: float64(last.DiskTotal), Format: fmtBytes,
				Series: []chartSeries{{Name: "Disco usado", Class: "s1", Pts: downsample(pts, b, func(p store.HostPoint) float64 { return float64(p.DiskUsed) }, true)}}},
		}
	}
	s.render(w, r, "monitor", d)
}
