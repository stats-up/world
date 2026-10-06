package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestMetricsRollup(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	base := time.Now().Unix()/ResQuarter*ResQuarter - ResQuarter
	// Dos muestras en el mismo minuto: CPU promediada, RAM al máximo.
	for _, p := range []CtrPoint{{Key: "a", TS: base, CPU: 1, Mem: 100}, {Key: "a", TS: base + 30, CPU: 0.5, Mem: 300, OOMKills: 2}} {
		if err := st.SaveCtrMetrics([]CtrPoint{p}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SaveCtrMetrics([]CtrPoint{{Key: "a", TS: base + 120, CPU: 0.25, Mem: 200}}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveHostMetric(HostPoint{TS: base, CPU: 40, MemUsed: 10, MemTotal: 20}); err != nil {
		t.Fatal(err)
	}
	s, _ := st.CtrSeries("a", ResMinute, 0)
	if got := s["a"]; len(got) != 2 || got[0].CPU != 0.75 || got[0].Mem != 300 || got[0].OOMKills != 2 {
		t.Fatalf("detalle por minuto: %+v", got)
	}
	if err := st.RollupMetrics(base, base+ResQuarter, time.Now()); err != nil {
		t.Fatal(err)
	}
	q, _ := st.CtrSeries("", ResQuarter, 0)
	if got := q["a"]; len(got) != 1 || got[0].TS != base || got[0].CPU != 0.5 || got[0].Mem != 300 {
		t.Fatalf("resumen 15 min: %+v", got)
	}
	h, _ := st.HostSeries(ResQuarter, 0)
	if len(h) != 1 || h[0].CPU != 40 {
		t.Fatalf("resumen del servidor: %+v", h)
	}
	// Lo vencido se borra.
	if err := st.RollupMetrics(base, base, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if s, _ := st.CtrSeries("a", ResMinute, 0); len(s["a"]) != 0 {
		t.Fatalf("el detalle de más de 48 h debía borrarse: %+v", s["a"])
	}
}
