package monitor

import "testing"

func TestParseProcStat(t *testing.T) {
	// user nice system idle iowait irq softirq steal guest guest_nice
	got, err := parseProcStat("cpu  100 0 50 800 50 0 0 0 30 0\ncpu0 1 2 3 4\n")
	if err != nil {
		t.Fatal(err)
	}
	if got.total != 1000 || got.busy != 150 {
		t.Fatalf("total=%d busy=%d; quería 1000 y 150", got.total, got.busy)
	}
}

func TestParseMeminfo(t *testing.T) {
	m := parseMeminfo("MemTotal:        2000 kB\nMemAvailable:     500 kB\nHugePages_Total:       0\n")
	if m["MemTotal"] != 2000*1024 || m["MemAvailable"] != 500*1024 || m["HugePages_Total"] != 0 {
		t.Fatalf("meminfo mal leído: %v", m)
	}
}
