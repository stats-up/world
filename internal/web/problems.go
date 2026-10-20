package web

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"world/internal/docker"
	"world/internal/store"
)

// Problemas: lo que está fallando ahora en el servidor, los sitios y el correo, en un solo lugar
// (resumen y monitoreo). Se calcula al abrir la página con lo que world ya sabe.

type problem struct {
	Level    string // err | warn | low (baja prioridad: ej. restos de pruebas que se dejan a propósito)
	Area     string // sitio o servicio
	Title    string
	Detail   string
	Link     string
	LinkText string
}

func (p problem) Err() bool { return p.Level == "err" }
func (p problem) Low() bool { return p.Level == "low" }

func (p problem) rank() int {
	switch p.Level {
	case "err":
		return 0
	case "low":
		return 2
	}
	return 1
}

// testSite: nombres que usan las pruebas de integración de world (ittest, ittest2, rntest-a…). Sus
// contenedores pueden quedar a propósito para seguir probando: se reportan con baja prioridad.
func testSite(name string) bool {
	return strings.HasPrefix(name, "ittest") || strings.HasPrefix(name, "rntest-")
}

// problemInput es lo que se revisa; se separa de cómo se obtiene para poder probarlo.
type problemInput struct {
	Health     healthReport
	Traefik    string
	Mon        monView
	Sites      []*store.Site
	Latest     map[int64]*store.Deployment // último deploy de cada sitio
	Containers map[string]docker.ContainerSummary
	MailCheck  func() []problem // bloqueos de Stalwart a las direcciones del servidor
}

// problems junta los problemas actuales, primero los graves. containers puede venir ya consultado.
func (s *Server) problems(ctx context.Context, containers map[string]docker.ContainerSummary, mon monView, health healthReport) []problem {
	in := problemInput{Health: health, Traefik: s.proxy.Status(ctx), Mon: mon, Containers: containers, Latest: map[int64]*store.Deployment{}}
	in.Sites, _ = s.st.Sites()
	for _, site := range in.Sites {
		if dep, err := s.st.LatestDeployment(site.ID); err == nil {
			in.Latest[site.ID] = dep
		}
	}
	in.MailCheck = func() []problem {
		mctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		blocked, err := s.mailAPI.BlockedIPs(mctx)
		if err != nil {
			return nil
		}
		var out []problem
		server := s.serverAddrs(mctx)
		for _, b := range blocked {
			if l := serverLabel(b.Address, server); l != "" {
				out = append(out, problem{"err", "Correo", "Stalwart bloquea una dirección del servidor (" + b.Address + ")",
					l + ": los sitios que llegan por ahí no pueden enviar correo.", "/mail/security", "Desbloquear"})
			}
		}
		return out
	}
	return buildProblems(in)
}

func buildProblems(in problemInput) []problem {
	health, mon, containers := in.Health, in.Mon, in.Containers
	var out []problem
	add := func(level, area, title, detail, link, linkText string) {
		out = append(out, problem{level, area, title, detail, link, linkText})
	}

	// Servicios internos de world.
	if !health.AutoDeploy.OK && health.AutoDeploy.SecondsAgo >= 0 {
		add("err", "world", "El auto-deploy de los sitios está detenido", health.AutoDeploy.Error, "", "")
	}
	if !health.SelfUpdate.OK {
		add("warn", "world", "El auto-update del panel no está corriendo", health.SelfUpdate.Error, "", "")
	}
	if !health.SiteCron.OK && health.SiteCron.SecondsAgo >= 0 {
		add("err", "world", "El cron de los sitios está detenido", health.SiteCron.Error, "", "")
	}
	if st := in.Traefik; st != "running" {
		add("err", "Traefik", "El proxy no está corriendo ("+st+")", "Ningún sitio responde por HTTPS mientras Traefik esté detenido.", "", "")
	}

	// Servidor.
	for _, b := range mon.Bars {
		if b.Label == "CPU" || b.Level == "ok" {
			continue // la CPU varía mucho de un momento a otro: se mira en los gráficos
		}
		add(b.Level, "Servidor", fmt.Sprintf("%s al %.0f %%", b.Label, b.Pct), b.Text, "/monitor", "Ver monitoreo")
	}

	// Sitios.
	names := map[string]bool{}
	for _, site := range in.Sites {
		names[site.Name] = true
		link := fmt.Sprintf("/sites/%d", site.ID)
		dep := in.Latest[site.ID]
		if dep != nil && dep.Running() {
			continue // en pleno deploy: el estado cambia en segundos
		}
		depLink, depText := link, "Ver sitio"
		if dep != nil {
			depLink, depText = fmt.Sprintf("/deployments/%d", dep.ID), fmt.Sprintf("Ver deploy #%d", dep.ID)
		}
		c, has := containers[site.Name]
		running := has && c.State == "running"
		switch {
		case containers == nil:
			// Docker no respondió: el resumen ya lo avisa.
		case !has && dep == nil:
			add("warn", site.Name, "Nunca se ha desplegado", "El sitio existe en world pero no tiene contenedor.", link, "Ver sitio")
		case !has && dep.Status == store.DeployFailed:
			add("err", site.Name, "No está publicado: el último deploy falló", firstLine(dep.Error), depLink, depText)
		case !has:
			add("err", site.Name, "No tiene contenedor", "Haz un deploy para volver a publicarlo.", link, "Ver sitio")
		case !running:
			add("err", site.Name, "El contenedor está detenido ("+c.State+")", c.Status, link, "Ver sitio")
		case strings.Contains(c.Status, "unhealthy"):
			add("err", site.Name, "El contenedor no está saludable", "El health check de la imagen está fallando: "+c.Status, link, "Ver sitio")
		}
		if running && dep != nil && dep.Status == store.DeployFailed {
			add("warn", site.Name, fmt.Sprintf("El último deploy (#%d) falló", dep.ID),
				"Sigue atendiendo la versión anterior. "+firstLine(dep.Error), depLink, depText)
		}
		if running && site.CronCommand != "" && !site.LastCronAt.IsZero() && site.LastCronExit != 0 &&
			time.Since(site.LastCronAt) < 15*time.Minute {
			detail := firstLine(site.LastCronOutput)
			if site.LastCronExit == 127 {
				detail = "El comando no existe dentro del contenedor (" + detail + ")."
				if site.Kind != store.KindPHP {
					detail += " El sitio no es PHP/Laravel: si no necesita cron, deja el comando vacío."
				}
			}
			add("err", site.Name, fmt.Sprintf("El cron falla (código %d)", site.LastCronExit),
				"«"+site.CronCommand+"»: "+detail, link+"/edit", "Editar el cron")
		}
		if site.AutoDeploy && site.LastCheckError != "" {
			add("warn", site.Name, "El auto-deploy no puede revisar la rama", firstLine(site.LastCheckError), link, "Ver sitio")
		}
		if row := mon.Sites[site.Name]; running && row != nil {
			if row.OOMKills > 0 {
				add("warn", site.Name, fmt.Sprintf("%d proceso(s) terminados por falta de memoria", row.OOMKills),
					"Desde que inició el contenedor. Sube el límite de RAM del sitio si se repite.", link, "Ver sitio")
			}
			if p := row.MemPct(); p >= 90 {
				add("warn", site.Name, fmt.Sprintf("Usa el %.0f %% de su límite de RAM", p), row.MemText()+" de "+row.LimitText(), link, "Ver sitio")
			}
		}
	}
	// Contenedores de sitios que ya no existen en world (ej: restos de pruebas).
	for name, c := range containers {
		if !names[name] && testSite(name) {
			add("low", name, "Contenedor de pruebas sin sitio en world", strings.Join(c.Names, ", ")+" ("+c.State+") quedó de las pruebas de world y se deja para seguir probando. Bórralo cuando ya no se use.", "", "")
		} else if !names[name] {
			add("warn", name, "Contenedor sin sitio en world", strings.Join(c.Names, ", ")+" ("+c.State+") no corresponde a ningún sitio: revísalo y bórralo si sobra.", "", "")
		}
	}

	// Correo.
	if health.Mail != nil {
		if !health.Mail.OK {
			add("err", "Correo", "Stalwart no está funcionando ("+health.Mail.State+")", "Los sitios no pueden enviar correo.", "/mail", "Ver correo")
		} else if in.MailCheck != nil {
			out = append(out, in.MailCheck()...)
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].rank() < out[j].rank() })
	return out
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200]) + "…"
	}
	return s
}
