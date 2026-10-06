package store

import (
	"path/filepath"
	"testing"
)

func TestMailImports(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, err := st.CreateMailImport(&MailImport{AccountID: "a", Email: "x@y.cl", Host: "38.18.230.11", Port: 993, SrcUser: "x@y.cl"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetMailImportProgress(id, 10, 8, 40, 0); err != nil {
		t.Fatal(err)
	}
	m, err := st.MailImport(id)
	if err != nil || !m.Running() || m.Percent() != 25 || m.Skipped() != 2 {
		t.Fatalf("en curso: %+v %v", m, err)
	}
	if run, _ := st.RunningMailImports(); len(run) != 1 {
		t.Fatalf("corriendo: %d", len(run))
	}
	// Cancelada: el resultado posterior del contenedor no la pisa.
	st.FinishMailImport(id, ImportCanceled, "")
	st.FinishMailImport(id, ImportFailed, "código 137")
	m, _ = st.MailImport(id)
	if m.Status != ImportCanceled || m.Error != "" || m.FinishedAt.IsZero() {
		t.Fatalf("cancelada: %+v", m)
	}
	id2, _ := st.CreateMailImport(&MailImport{AccountID: "a", Email: "x@y.cl", Host: "h", Port: 993, SrcUser: "x"})
	list, _ := st.MailImports("a", 5)
	if len(list) != 2 || list[0].ID != id2 {
		t.Fatalf("historial: %+v", list)
	}
	if other, _ := st.MailImports("b", 5); len(other) != 0 {
		t.Fatal("no debe mezclar buzones")
	}
}
