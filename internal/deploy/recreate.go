package deploy

import (
	"context"
	"fmt"
	"strings"

	"world/internal/store"
)

// Deploy "recreate": para servicios que no admiten dos instancias a la vez (colas, sesiones de
// WhatsApp, bots). La imagen ya está construida; aquí se pausa el cron, se detienen los contenedores
// actuales y, si el nuevo no queda sano, se vuelven a levantar. Los detenidos se eliminan después,
// en "Retirando versión anterior", igual que en el deploy normal.

type recreateState struct {
	resume  func() // reanuda el cron del sitio
	restart func() // vuelve a levantar los contenedores detenidos (si el nuevo falla)
}

func (e *Engine) recreateStop(ctx context.Context, lg *logger, site *store.Site) (*recreateState, error) {
	st := &recreateState{resume: func() {}, restart: func() {}}
	if e.cron != nil {
		lg.Step("Esperando que termine el cron")
		release, waited, err := e.cron.hold(ctx, site.ID, cronWait)
		if err != nil {
			return nil, fmt.Errorf("%w: se canceló el deploy sin tocar el contenedor actual", err)
		}
		st.resume = release
		if waited {
			lg.Printf("El cron terminó; queda en pausa hasta el final del deploy.")
		} else {
			lg.Printf("El cron no estaba en ejecución; queda en pausa hasta el final del deploy.")
		}
	}

	lg.Step("Deteniendo la versión actual")
	list, err := e.dc.ContainersByLabel(ctx, labelSite+"="+site.Name)
	if err != nil {
		st.resume()
		return nil, err
	}
	var stopped []string
	for _, c := range list {
		if c.State != "running" {
			continue
		}
		lg.Printf("Deteniendo %s (espera hasta %s a que cierre)", strings.Join(c.Names, ","), site.StopWait())
		if err := e.dc.ContainerStop(ctx, c.ID, site.StopWait()); err != nil {
			for _, id := range stopped {
				e.dc.ContainerStart(ctx, id)
			}
			st.resume()
			return nil, fmt.Errorf("no se pudo detener %s: %w", strings.Join(c.Names, ","), err)
		}
		stopped = append(stopped, c.ID)
	}
	if len(stopped) == 0 {
		lg.Printf("No había contenedor corriendo.")
	}
	st.restart = func() {
		for _, id := range stopped {
			lg.Printf("Volviendo a levantar la versión anterior (%s)", id[:12])
			if err := e.dc.ContainerStart(context.Background(), id); err != nil {
				lg.Printf("No se pudo levantar %s: %v", id[:12], err)
			}
		}
	}
	return st, nil
}
