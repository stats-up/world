package phpcheck

import (
	"strings"
	"testing"
)

func TestParseReleases(t *testing.T) {
	body := `{"8":{"8.2":{"version":"8.2.35","tags":["security"]},"8.3":{"version":"8.3.36","tags":["security"]},"8.4":{"version":"8.4.27","tags":[]}}}`
	rel, sup, err := parseReleases(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if rel["8.3"].Version != "8.3.36" || !rel["8.3"].Security || rel["8.4"].Security || !sup["8.2"] || sup["8.1"] {
		t.Fatalf("rel=%v sup=%v", rel, sup)
	}
	if _, _, err := parseReleases(strings.NewReader(`{}`)); err == nil {
		t.Fatal("una lista vacía es un error")
	}
}

func TestLessAndStatus(t *testing.T) {
	for _, c := range []struct {
		a, b string
		less bool
	}{{"8.3.35", "8.3.36", true}, {"8.3.36", "8.3.36", false}, {"8.3.9", "8.3.10", true}, {"8.4.1", "8.3.99", false}} {
		if Less(c.a, c.b) != c.less {
			t.Errorf("Less(%s, %s) != %v", c.a, c.b, c.less)
		}
	}
	waiting := Status{Running: "8.3.35", Latest: "8.3.36", Image: "8.3.35"}
	if !waiting.Outdated() || waiting.NeedsDeploy() || waiting.ImageReady() {
		t.Errorf("esperando la imagen: %+v", waiting)
	}
	ready := Status{Running: "8.3.35", Latest: "8.3.36", Image: "8.3.36"}
	if !ready.NeedsDeploy() || !ready.ImageReady() {
		t.Errorf("imagen lista: %+v", ready)
	}
	if (Status{Running: "8.3.36", Latest: "8.3.36", Image: "8.3.36"}).Outdated() {
		t.Error("al día no está desactualizado")
	}
}
