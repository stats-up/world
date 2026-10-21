package web

import (
	"html/template"
	"strings"
	"testing"
	"time"
	"world/internal/phpcheck"

	"world/internal/docker"
	"world/internal/store"
)

func TestBuildProblems(t *testing.T) {
	now := time.Now()
	ok := cronStatus{OK: true, SecondsAgo: 5}
	failed := &store.Deployment{ID: 60, Status: store.DeployFailed, Error: "el health check del contenedor falló"}
	in := problemInput{
		Health:  healthReport{AutoDeploy: ok, SelfUpdate: ok, SiteCron: ok},
		Traefik: "running",
		Mon: monView{Bars: []monBar{{Label: "CPU", Pct: 99, Level: "err"}, {Label: "Disco", Pct: 91, Level: "err", Text: "36 GB de 40 GB"}},
			Sites: map[string]*ctrRow{"tienda": {Mem: 470 << 20, MemLimit: 512 << 20, OOMKills: 2}}},
		Sites: []*store.Site{
			{ID: 1, Name: "tienda", Kind: store.KindPHP},
			{ID: 3, Name: "webmail", Kind: store.KindDockerfile, CronCommand: "php artisan schedule:run",
				LastCronAt: now.Add(-time.Minute), LastCronExit: 127, LastCronOutput: "sh: php: not found\n"},
			{ID: 8, Name: "candidatos", Kind: store.KindPHP},
			{ID: 9, Name: "nuevo"},
			{ID: 10, Name: "deployando"},
		},
		Latest: map[int64]*store.Deployment{3: {ID: 29, Status: store.DeploySuccess}, 8: failed, 1: {ID: 70, Status: store.DeployFailed, Error: "docker build falló\nmás"},
			10: {ID: 71, Status: store.DeployRunning}},
		Containers: map[string]docker.ContainerSummary{
			"tienda":  {State: "running", Status: "Up 2 hours (healthy)"},
			"webmail": {State: "running", Status: "Up 2 days"},
			"ittest":  {State: "running", Names: []string{"/world-ittest-2"}},
			"viejo":   {State: "exited", Names: []string{"/world-viejo-3"}},
		},
	}
	got := buildProblems(in)
	var lines []string
	for _, p := range got {
		lines = append(lines, p.Level+" "+p.Area+": "+p.Title+" | "+p.Detail)
	}
	all := strings.Join(lines, "\n")
	for _, want := range []string{
		"err webmail: El cron falla (código 127) | «php artisan schedule:run»: El comando no existe dentro del contenedor (sh: php: not found). El sitio no es PHP/Laravel",
		"err candidatos: No está publicado: el último deploy falló | el health check del contenedor falló",
		"err Servidor: Disco al 91 %",
		"warn tienda: El último deploy (#70) falló | Sigue atendiendo la versión anterior. docker build falló",
		"warn tienda: 2 proceso(s) terminados por falta de memoria",
		"warn tienda: Usa el 92 % de su límite de RAM",
		"warn nuevo: Nunca se ha desplegado",
		"low ittest: Contenedor de pruebas sin sitio en world | /world-ittest-2",
		"warn viejo: Contenedor sin sitio en world | /world-viejo-3",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("falta %q en:\n%s", want, all)
		}
	}
	if last := got[len(got)-1]; !last.Low() {
		t.Errorf("los de baja prioridad van al final, el último es %+v", last)
	}
	if strings.Contains(all, "CPU") || strings.Contains(all, "deployando") || strings.Contains(all, "Traefik") || strings.Contains(all, "world:") {
		t.Errorf("sobran problemas:\n%s", all)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Err() && !got[i-1].Err() {
			t.Fatal("los graves deben ir primero")
		}
	}
	// Servicios caídos.
	in = problemInput{Traefik: "exited", Health: healthReport{AutoDeploy: cronStatus{SecondsAgo: 900, Error: "x"}, SelfUpdate: ok, SiteCron: cronStatus{SecondsAgo: -1}}}
	all = ""
	for _, p := range buildProblems(in) {
		all += p.Title + "\n"
	}
	if !strings.Contains(all, "proxy no está corriendo (exited)") || !strings.Contains(all, "auto-deploy de los sitios está detenido") || strings.Contains(all, "cron de los sitios") {
		t.Errorf("servicios:\n%s", all)
	}

	// Se muestran en el resumen y en monitoreo.
	s := &Server{pages: map[string]*template.Template{}}
	if err := s.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := s.pages["monitor"].ExecuteTemplate(&out, "layout", data{"Problems": got[:2], "Range": siteRanges[0], "Ranges": siteRanges, "CSRF": "t"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `id="problemas"`) || !strings.Contains(out.String(), `href="/sites/3/edit"`) || !strings.Contains(out.String(), "Editar el cron") {
		t.Errorf("monitor sin problemas visibles")
	}
	out.Reset()
	s.pages["monitor"].ExecuteTemplate(&out, "layout", data{"ProblemsAll": true, "Range": siteRanges[0], "Ranges": siteRanges, "CSRF": "t"})
	if !strings.Contains(out.String(), "Sin problemas") {
		t.Error("debió decir que no hay problemas")
	}
}

func TestPHPProblems(t *testing.T) {
	site := &store.Site{ID: 4, Name: "tienda", Kind: store.KindPHP, PHPVersion: "8.3"}
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		st     phpcheck.Status
		titles []string
	}{
		{phpcheck.Status{Branch: "8.1", Running: "8.1.34", Known: true}, []string{"err PHP 8.1 ya no recibe parches de seguridad (desde 31-12-2025)"}},
		// Sin datos de php.net ni del contenedor igual se sabe por la fecha.
		{phpcheck.Status{Branch: "8.1"}, []string{"err PHP 8.1 ya no recibe parches de seguridad (desde 31-12-2025)"}},
		{phpcheck.Status{Branch: "8.2", Running: "8.2.34", Latest: "8.2.35", Image: "8.2.34", Supported: true, Security: true, Known: true},
			[]string{"warn PHP 8.2 deja de recibir parches el 31-12-2026 (en 71 días)", "warn PHP 8.2.34 tiene una versión nueva (8.2.35) con parches de seguridad"}},
		{phpcheck.Status{Branch: "8.3", Running: "8.3.35", Latest: "8.3.36", Image: "8.3.36", Supported: true, Security: true, Known: true}, []string{"err PHP 8.3.35 → 8.3.36 disponible"}},
		{phpcheck.Status{Branch: "8.3", Running: "8.3.36", Latest: "8.3.36", Image: "8.3.36", Supported: true, Known: true}, nil},
		{phpcheck.Status{Branch: "8.5"}, nil},
	}
	for _, c := range cases {
		var got []string
		for _, p := range phpProblems(site, c.st, now) {
			got = append(got, p.Level+" "+p.Title)
		}
		if strings.Join(got, "|") != strings.Join(c.titles, "|") {
			t.Errorf("%+v: got %q, want %q", c.st, got, c.titles)
		}
	}
}
