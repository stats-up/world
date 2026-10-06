package web

import (
	"fmt"
	"html/template"
	"math"
	"strconv"
	"strings"
	"time"
)

// Gráficos SVG armados en el servidor: sin librerías ni CDN (la CSP no permite scripts externos).

type chartPt struct {
	T int64
	V float64
}

type chartSeries struct {
	Name  string
	Class string // color: s1, s2 (definidos en app.css)
	Pts   []chartPt
}

type chart struct {
	Title    string
	From, To int64
	Res      int64 // segundos entre puntos; un hueco mayor corta la línea
	Series   []chartSeries
	Limit    float64 // línea de límite (0 = sin línea)
	MaxY     float64 // mínimo del eje Y (se amplía si los datos lo superan)
	Format   func(float64) string
	Markers  []int64 // ej: deploys
}

const (
	chartW, chartH = 560.0, 180.0
	padL, padR     = 62.0, 8.0
	padT, padB     = 10.0, 24.0
)

func (c chart) SVG() template.HTML {
	maxY := c.MaxY
	for _, s := range c.Series {
		for _, p := range s.Pts {
			maxY = math.Max(maxY, p.V)
		}
	}
	maxY = math.Max(maxY, c.Limit) * 1.08
	if maxY <= 0 {
		maxY = 1
	}
	span := float64(c.To - c.From)
	x := func(t int64) float64 { return padL + (float64(t-c.From)/span)*(chartW-padL-padR) }
	y := func(v float64) float64 { return padT + (1-v/maxY)*(chartH-padT-padB) }

	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %g %g" role="img" aria-label="%s">`, chartW, chartH, template.HTMLEscapeString(c.Title))
	// Grilla y eje Y (4 divisiones)
	for i := 0; i <= 4; i++ {
		v := maxY * float64(i) / 4
		fmt.Fprintf(&b, `<line class="grid" x1="%g" x2="%g" y1="%.1f" y2="%.1f"/>`, padL, chartW-padR, y(v), y(v))
		if i > 0 {
			fmt.Fprintf(&b, `<text class="axis" x="%g" y="%.1f" text-anchor="end">%s</text>`, padL-6, y(v)+4, template.HTMLEscapeString(c.Format(v)))
		}
	}
	// Eje X: 6 marcas de tiempo
	layout := "15:04"
	if span > 2*86400 {
		layout = "02/01"
	}
	for i := 0; i <= 6; i++ {
		t := c.From + int64(span*float64(i)/6)
		anchor := "middle"
		if i == 0 {
			anchor = "start"
		} else if i == 6 {
			anchor = "end"
		}
		fmt.Fprintf(&b, `<text class="axis" x="%.1f" y="%g" text-anchor="%s">%s</text>`, x(t), chartH-4, anchor, time.Unix(t, 0).Format(layout))
	}
	for _, m := range c.Markers {
		if m >= c.From && m <= c.To {
			fmt.Fprintf(&b, `<line class="marker" x1="%.1f" x2="%.1f" y1="%g" y2="%g"><title>Deploy %s</title></line>`,
				x(m), x(m), padT, chartH-padB, time.Unix(m, 0).Format("02/01 15:04"))
		}
	}
	if c.Limit > 0 {
		fmt.Fprintf(&b, `<line class="limit" x1="%g" x2="%g" y1="%.1f" y2="%.1f"><title>Límite: %s</title></line>`,
			padL, chartW-padR, y(c.Limit), y(c.Limit), template.HTMLEscapeString(c.Format(c.Limit)))
	}
	for _, s := range c.Series {
		if d := pathD(s.Pts, c.Res, x, y); d != "" {
			fmt.Fprintf(&b, `<path class="line %s" d="%s"><title>%s</title></path>`, s.Class, d, template.HTMLEscapeString(s.Name))
		}
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// pathD arma la línea cortándola donde faltan datos (panel o contenedor detenido).
func pathD(pts []chartPt, res int64, x func(int64) float64, y func(float64) float64) string {
	var b strings.Builder
	var prev int64
	for i, p := range pts {
		cmd := "L"
		if i == 0 || p.T-prev > 2*res {
			cmd = "M"
		}
		fmt.Fprintf(&b, "%s%.1f %.1f", cmd, x(p.T), y(p.V))
		prev = p.T
	}
	return b.String()
}

// sparkline es un minigráfico sin ejes para las tablas.
func sparkline(pts []chartPt, from, to, res int64, maxY float64) template.HTML {
	const w, h = 120.0, 28.0
	for _, p := range pts {
		maxY = math.Max(maxY, p.V)
	}
	if maxY <= 0 {
		maxY = 1
	}
	span := float64(to - from)
	x := func(t int64) float64 { return float64(t-from) / span * w }
	y := func(v float64) float64 { return 1 + (1-v/maxY)*(h-2) }
	d := pathD(pts, res, x, y)
	if d == "" {
		return ""
	}
	return template.HTML(fmt.Sprintf(`<svg class="spark" viewBox="0 0 %g %g" preserveAspectRatio="none" aria-hidden="true"><path d="%s"/></svg>`, w, h, d))
}

// --- Formatos ---

func fmtBytes(v float64) string {
	switch {
	case v >= 1<<30:
		return strconv.FormatFloat(v/(1<<30), 'f', 1, 64) + " GB"
	case v >= 1<<20:
		return strconv.FormatFloat(v/(1<<20), 'f', 0, 64) + " MB"
	case v >= 1<<10:
		return strconv.FormatFloat(v/(1<<10), 'f', 0, 64) + " KB"
	}
	return strconv.FormatFloat(v, 'f', 0, 64) + " B"
}

func fmtRate(v float64) string { return fmtBytes(v) + "/s" }

func fmtPct(v float64) string {
	if v < 10 && v != 0 {
		return strconv.FormatFloat(v, 'f', 1, 64) + " %"
	}
	return strconv.FormatFloat(v, 'f', 0, 64) + " %"
}
