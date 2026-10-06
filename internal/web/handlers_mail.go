package web

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"time"

	"world/internal/mail"
)

// Nombres de bucket S3 sin puntos (con puntos falla el TLS de los endpoints virtual-host).
var bucketRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)

// mailSettingsData agrega a los ajustes lo necesario para la sección Correo (sin exponer secretos).
func (s *Server) mailSettingsData(ms mail.Settings) data {
	return data{
		"Enabled":   ms.Enabled,
		"Hostname":  ms.Hostname,
		"S3Bucket":  ms.S3Bucket,
		"S3Prefix":  ms.S3Prefix,
		"HasS3":     ms.S3AccessKey != "" && ms.S3SecretKey != "",
		"HasCF":     ms.CFToken != "",
		"HasAdmin":  ms.AdminSecret != "",
		"AdminUser": mail.AdminUser,
		"Missing":   strings.Join(ms.Missing(), ", "),
	}
}

func (s *Server) mailSettingsSubmit(w http.ResponseWriter, r *http.Request) {
	ms := mail.Settings{
		Enabled:     r.FormValue("enabled") == "1",
		Hostname:    strings.ToLower(strings.TrimSpace(r.FormValue("hostname"))),
		S3Bucket:    strings.TrimSpace(r.FormValue("s3_bucket")),
		S3Prefix:    strings.TrimSpace(r.FormValue("s3_prefix")),
		S3AccessKey: strings.TrimSpace(r.FormValue("s3_access_key")),
		S3SecretKey: strings.TrimSpace(r.FormValue("s3_secret_key")),
		CFToken:     strings.TrimSpace(r.FormValue("cf_token")),
		AdminSecret: r.FormValue("admin_secret"),
	}
	fail := func(msg string) {
		s.setFlash(w, "error", msg)
		http.Redirect(w, r, "/settings#correo", http.StatusSeeOther)
	}
	switch {
	case ms.Hostname != "" && !hostRe.MatchString(ms.Hostname):
		fail("Nombre del servidor de correo inválido.")
		return
	case ms.S3Bucket != "" && !bucketRe.MatchString(ms.S3Bucket):
		fail("Nombre de bucket inválido: solo minúsculas, números y guiones.")
		return
	case ms.S3Prefix != "" && (strings.HasPrefix(ms.S3Prefix, "/") || !strings.HasSuffix(ms.S3Prefix, "/")):
		fail("El prefijo debe terminar en / y no empezar con / (ej: mail/).")
		return
	case ms.AdminSecret != "" && len(ms.AdminSecret) < 16:
		fail("La contraseña de administrador debe tener al menos 16 caracteres.")
		return
	}
	if err := mail.SaveSettings(s.st, s.box, ms); err != nil {
		fail("No se pudo guardar: " + err.Error())
		return
	}
	saved := mail.LoadSettings(s.st, s.box)
	if saved.Enabled {
		if missing := saved.Missing(); len(missing) > 0 {
			// No dejar el correo "activado" a medias: el EnsureLoop reintentaría sin fin.
			mail.Disable(s.st)
			fail("Guardado, pero el correo no se activó. Falta: " + strings.Join(missing, ", ") + ".")
			return
		}
	}

	// La primera vez descarga la imagen (~100 MB).
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	switch err := s.mail.Ensure(ctx, saved); {
	case err != nil:
		s.setFlash(w, "error", "Ajustes guardados, pero Stalwart no se pudo actualizar: "+err.Error())
	case saved.Enabled:
		s.setFlash(w, "ok", "Correo guardado. Stalwart está corriendo con esta configuración.")
	default:
		s.setFlash(w, "ok", "Correo guardado (desactivado). Los datos de Stalwart se conservan.")
	}
	http.Redirect(w, r, "/settings#correo", http.StatusSeeOther)
}
