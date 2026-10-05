package deploy

import (
	"path/filepath"
	"testing"

	"world/internal/store"
)

func TestShouldDeploy(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, err := st.CreateSite(&store.Site{Name: "a", RepoURL: "https://x/y.git", Branch: "main", Kind: store.KindStatic})
	if err != nil {
		t.Fatal(err)
	}
	shaA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	if !shouldDeploy(st, id, shaA) {
		t.Error("un sitio nunca desplegado debe desplegarse")
	}
	dep, _ := st.CreateDeployment(id, store.SourceAuto)
	if shouldDeploy(st, id, shaA) {
		t.Error("no debe lanzar otro deploy mientras uno corre")
	}
	st.FinishDeployment(dep, store.DeploySuccess, shaA, "")
	if shouldDeploy(st, id, shaA) {
		t.Error("el mismo commit ya desplegado no debe redesplegarse")
	}
	if !shouldDeploy(st, id, shaB) {
		t.Error("un commit nuevo debe desplegarse")
	}
	dep, _ = st.CreateDeployment(id, store.SourceAuto)
	st.FinishDeployment(dep, store.DeployFailed, shaB, "falló")
	if shouldDeploy(st, id, shaB) {
		t.Error("un commit que ya falló no debe reintentarse en bucle")
	}
	dep, _ = st.CreateDeployment(id, store.SourceAuto)
	st.FinishDeployment(dep, store.DeployFailed, "", "git clone falló")
	if shouldDeploy(st, id, shaB) {
		t.Error("tras un fallo sin commit debe esperar antes de reintentar")
	}
	// Compatibilidad con deploys antiguos que guardaban el SHA abreviado.
	dep, _ = st.CreateDeployment(id, store.SourceManual)
	st.FinishDeployment(dep, store.DeploySuccess, shaA[:12], "")
	if shouldDeploy(st, id, shaA) {
		t.Error("el SHA abreviado debe reconocerse como el mismo commit")
	}
}
