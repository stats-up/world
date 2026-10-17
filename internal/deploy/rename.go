package deploy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"world/internal/docker"
	"world/internal/store"
)

// Renombrar un sitio es un deploy con el nombre nuevo: la imagen se construye mientras el sitio
// sigue atendiendo; recién al cambiar se detiene el contenedor actual, se copian sus carpetas
// persistentes a volúmenes con el nombre nuevo y se levanta el nuevo. Si algo falla antes de que
// el nuevo quede funcionando, se vuelve a levantar el actual y nada cambia.

// copyImage es la imagen auxiliar que copia los volúmenes.
const copyImage = "alpine:3.20"

// renameCheck valida, antes de construir nada, que el nombre nuevo esté libre en world y en Docker.
func (e *Engine) renameCheck(ctx context.Context, site *store.Site, newName string) error {
	if newName == site.Name {
		return errors.New("el nombre nuevo es igual al actual")
	}
	if taken, err := e.st.SiteNameTaken(newName); err != nil {
		return err
	} else if taken {
		return fmt.Errorf("ya existe un sitio llamado %s", newName)
	}
	if list, err := e.dc.ContainersByLabel(ctx, labelSite+"="+newName); err != nil {
		return err
	} else if len(list) > 0 {
		return fmt.Errorf("ya hay contenedores de un sitio %s en Docker", newName)
	}
	for _, path := range site.PersistPaths {
		vol := VolumeName(newName, path)
		if exists, err := e.dc.VolumeExists(ctx, vol); err != nil {
			return err
		} else if exists {
			return fmt.Errorf("ya existe el volumen %s (quizás de un sitio eliminado con ese nombre): bórralo a mano o elige otro nombre", vol)
		}
	}
	return nil
}

// renameSwitch detiene el contenedor actual y copia sus volúmenes a los del nombre nuevo.
// Devuelve undo, que vuelve a levantar el contenedor actual y quita las copias.
func (e *Engine) renameSwitch(ctx context.Context, lg *logger, site *store.Site, oldName string) (func(), error) {
	olds, err := e.dc.ContainersByLabel(ctx, labelSite+"="+oldName)
	if err != nil {
		return nil, err
	}
	var copies []string
	undo := func() {
		// Con un contexto propio: si el deploy se canceló o venció, igual hay que dejar el sitio arriba.
		uctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		lg.Step("Volviendo a levantar " + oldName + " (el nombre no cambió)")
		for _, vol := range copies {
			if err := e.dc.VolumeRemove(uctx, vol); err != nil {
				lg.Printf("No se pudo quitar la copia %s: %v", vol, err)
			}
		}
		for _, c := range olds {
			if c.State != "running" {
				continue
			}
			if err := e.dc.ContainerStart(uctx, c.ID); err != nil {
				lg.Printf("✗ No se pudo iniciar %s: %v", strings.Join(c.Names, ","), err)
			} else {
				lg.Printf("Iniciado %s", strings.Join(c.Names, ","))
			}
		}
	}
	if len(site.PersistPaths) > 0 {
		if err := e.dc.ImagePull(ctx, copyImage); err != nil {
			return nil, err
		}
	}
	lg.Step("Deteniendo " + oldName + " para copiar sus datos")
	for _, c := range olds {
		if c.State == "running" {
			if err := e.dc.ContainerStop(ctx, c.ID, 15*time.Second); err != nil {
				undo()
				return nil, err
			}
			lg.Printf("Detenido %s", strings.Join(c.Names, ","))
		}
	}
	for _, path := range site.PersistPaths {
		from, to := VolumeName(oldName, path), VolumeName(site.Name, path)
		exists, err := e.dc.VolumeExists(ctx, from)
		if err != nil {
			undo()
			return nil, err
		}
		if !exists {
			lg.Printf("%s: no había volumen, se crea vacío", path)
			continue
		}
		start := time.Now()
		copies = append(copies, to)
		if err := e.copyVolume(ctx, from, to); err != nil {
			undo()
			return nil, fmt.Errorf("copiando %s: %w", path, err)
		}
		lg.Printf("Copiado %s → %s (%s)", from, to, time.Since(start).Round(time.Millisecond))
	}
	return undo, nil
}

// copyVolume copia el contenido de un volumen a otro nuevo (vacío), conservando dueños y permisos.
func (e *Engine) copyVolume(ctx context.Context, from, to string) error {
	id, err := e.dc.ContainerCreate(ctx, "", docker.ContainerConfig{
		Image:  copyImage,
		Cmd:    []string{"sh", "-c", `[ -z "$(ls -A /to)" ] || { echo "el destino no está vacío" >&2; exit 3; }; cp -a /from/. /to/`},
		Labels: map[string]string{"world.managed": "rename-copy"},
		HostConfig: docker.HostConfig{
			Binds: []string{from + ":/from:ro", to + ":/to"},
		},
	})
	if err != nil {
		return err
	}
	defer e.dc.ContainerRemove(context.Background(), id)
	if err := e.dc.ContainerStart(ctx, id); err != nil {
		return err
	}
	for {
		info, err := e.dc.ContainerInspect(ctx, id)
		if err != nil {
			return err
		}
		if !info.State.Running && info.State.FinishedAt != "" && !strings.HasPrefix(info.State.FinishedAt, "0001") {
			if info.State.ExitCode != 0 {
				logs, _ := e.dc.ContainerLogs(ctx, id, 20)
				return fmt.Errorf("código %d: %s", info.State.ExitCode, strings.TrimSpace(logs))
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// renameCleanup quita lo que quedó con el nombre anterior: contenedores, imágenes, volúmenes
// (ya copiados) y la carpeta de la deploy key. Los errores solo se anotan: el sitio ya funciona.
func (e *Engine) renameCleanup(ctx context.Context, lg *logger, site *store.Site, oldName string) {
	lg.Step("Limpiando lo que quedó con el nombre " + oldName)
	if olds, err := e.dc.ContainersByLabel(ctx, labelSite+"="+oldName); err == nil {
		for _, c := range olds {
			if err := e.dc.ContainerRemove(ctx, c.ID); err != nil {
				lg.Printf("No se pudo eliminar %s: %v", strings.Join(c.Names, ","), err)
				continue
			}
			lg.Printf("Eliminado %s", strings.Join(c.Names, ","))
		}
	}
	if imgs, err := e.dc.ImagesByReference(ctx, "world/"+oldName); err == nil {
		for _, img := range imgs {
			for _, tag := range img.RepoTags {
				if err := e.dc.ImageRemove(ctx, tag); err == nil {
					lg.Printf("Imagen eliminada: %s", tag)
				}
			}
		}
	}
	for _, path := range site.PersistPaths {
		vol := VolumeName(oldName, path)
		if exists, _ := e.dc.VolumeExists(ctx, vol); !exists {
			continue
		}
		if err := e.dc.VolumeRemove(ctx, vol); err != nil {
			lg.Printf("No se pudo eliminar el volumen %s: %v", vol, err)
		} else {
			lg.Printf("Volumen eliminado: %s (sus datos están en %s)", vol, VolumeName(site.Name, path))
		}
	}
	os.RemoveAll(e.cfg.Path("sites", oldName))
}
