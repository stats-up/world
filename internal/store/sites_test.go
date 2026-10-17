package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestRenameSite(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, _ := st.CreateSite(&Site{Name: "admin-5s", RepoURL: "x", Branch: "main"})
	st.CreateSite(&Site{Name: "otro", RepoURL: "x", Branch: "main"})
	ts := time.Now().Unix() - 120
	// Métricas del sitio y otras viejas de un sitio eliminado que se llamaba como el nombre nuevo.
	st.SaveCtrMetrics([]CtrPoint{{Key: "admin-5s", TS: ts, Mem: 100}, {Key: "5s-admin", TS: ts - 3600, Mem: 999}})

	if taken, _ := st.SiteNameTaken("otro"); !taken {
		t.Error("otro está tomado")
	}
	if err := st.RenameSite(id, "otro"); err == nil {
		t.Error("debió fallar: nombre tomado")
	}
	if err := st.RenameSite(id, "5s-admin"); err != nil {
		t.Fatal(err)
	}
	if s, _ := st.Site(id); s.Name != "5s-admin" {
		t.Fatalf("nombre: %s", s.Name)
	}
	series, _ := st.CtrSeries("", ResMinute, 0)
	if got := series["5s-admin"]; len(got) != 1 || got[0].Mem != 100 || len(series["admin-5s"]) != 0 {
		t.Fatalf("métricas: %+v", series)
	}
}
