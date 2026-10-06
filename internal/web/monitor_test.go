package web

import (
	"html/template"
	"strings"
	"testing"
	"time"

	"world/internal/store"
)

func TestMonitorPagesRender(t *testing.T) {
	s := &Server{pages: map[string]*template.Template{}}
	if err := s.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	pts := []chartPt{{now - 3600, 10}, {now - 3000, 30}, {now - 60, 20}}
	row := &ctrRow{Key: "tienda", Name: "tienda", SiteID: 3, CPU: 12.4, Mem: 300 << 20, MemLimit: 512 << 20, OOMKills: 1,
		CPUSpark: sparkline(pts, now-86400, now, 600, 5), MemSpark: sparkline(pts, now-86400, now, 600, 5)}
	mon := monView{Ready: true, At: time.Now(), Load: "0.42 (2 vCPU)",
		Bars:     []monBar{{Label: "CPU", Pct: 35, Text: "35 %", Level: "ok"}, {Label: "RAM", Pct: 92, Text: "1.8 GB de 2.0 GB", Level: "err"}},
		Services: []ctrRow{{Key: "@traefik", Name: "Traefik (proxy y SSL)", Mem: 40 << 20}},
		Sites:    map[string]*ctrRow{"tienda": row}}
	c := chart{Title: "RAM", From: now - 86400, To: now, Res: 300, Limit: 512 << 20, Format: fmtBytes, Markers: []int64{now - 3500},
		Series: []chartSeries{{Name: "RAM usada", Class: "s1", Pts: pts}}}
	site := &store.Site{ID: 3, Name: "tienda", Kind: store.KindPHP}
	pages := map[string]data{
		"dashboard": {"Sites": []*store.Site{site}, "Mon": mon, "Docker": nil, "DockerError": "x"},
		"monitor":   {"Mon": mon, "Range": siteRanges[0], "Ranges": siteRanges, "Charts": []chart{c}},
		"site": {"Site": site, "Metrics": siteMetrics{Range: siteRanges[1], Ranges: siteRanges, Now: *row, HasNow: true,
			Charts: []chart{c}, OOMEvents: 2}},
	}
	for name, d := range pages {
		d["CSRF"] = "tok"
		var b strings.Builder
		if err := s.pages[name].ExecuteTemplate(&b, "layout", d); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out := b.String()
		if strings.Contains(out, "style=\"") {
			t.Errorf("%s: estilos inline (los bloquea la CSP)", name)
		}
		for _, want := range map[string][]string{
			"dashboard": {`id="monitor-host"`, "1.8 GB de 2.0 GB", `class="spark"`, "1 sin memoria", "Traefik"},
			"monitor":   {`class="chart"`, `href="/sites/3"`, "servicio", `class="limit"`, `class="marker"`},
			"site":      {`class="chart"`, "12 %", "512 MB", "terminó 2 proceso", `class="active"`},
		}[name] {
			if !strings.Contains(out, want) {
				t.Errorf("%s: falta %q", name, want)
			}
		}
	}
}

func TestDownsample(t *testing.T) {
	pts := []store.CtrPoint{{TS: 0, CPU: 1}, {TS: 60, CPU: 3}, {TS: 300, CPU: 5}}
	got := downsample(pts, 300, func(p store.CtrPoint) float64 { return p.CPU }, false)
	if len(got) != 2 || got[0].V != 2 || got[1].V != 5 || got[0].T != 150 {
		t.Fatalf("%+v", got)
	}
	if mx := downsample(pts, 300, func(p store.CtrPoint) float64 { return p.CPU }, true); mx[0].V != 3 {
		t.Fatalf("máximo: %+v", mx)
	}
	// Un hueco mayor a 2 bloques corta la línea.
	d := pathD([]chartPt{{0, 1}, {300, 1}, {3000, 1}}, 300, func(t int64) float64 { return float64(t) }, func(v float64) float64 { return v })
	if strings.Count(d, "M") != 2 {
		t.Fatalf("la línea debía cortarse: %s", d)
	}
}
