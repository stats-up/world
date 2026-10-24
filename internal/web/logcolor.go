package web

import (
	"html/template"
	"regexp"
	"strings"
)

// Colores de los logs (deploys y contenedores): cada línea se clasifica por su contenido.
var (
	ansiRe        = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
	logBuildCmdRe = regexp.MustCompile(`^#\d+ \[`)
	logStepRe     = regexp.MustCompile(`^(==> |--- .* ---$)`)
	logErrRe      = regexp.MustCompile(`(?i)^✗|\b(error|errors|fatal|failed|failure|exception|panic|critical|emergency)\b|\bfall(ó|aron|ido)\b|\bse detuvo\b|" 5\d\d `)
	logWarnRe     = regexp.MustCompile(`(?i)^⚠|\b(warn|warning|deprecated|notice)\b|" 4\d\d `)
	logOKRe       = regexp.MustCompile(`(?i)^✓|\bhealth check ok\b|\bcontenedor estable\b|^#\d+ DONE\b|\bDONE\b|\bnothing to migrate\b|\bsuccessfully\b`)
	logInfoRe     = regexp.MustCompile(`(?i)\bINFO\b|^🚀|🚀 `)
)

// logClass devuelve la clase de color de una línea ("" = sin color).
func logClass(line string) string {
	switch {
	case strings.TrimSpace(line) == "":
		return ""
	case logStepRe.MatchString(line):
		return "l-step"
	case logBuildCmdRe.MatchString(line): // BuildKit repite el comando del paso: no es un resultado
		return ""
	case strings.HasPrefix(line, "⚠"): // aviso explícito, aunque mencione un error
		return "l-warn"
	case logErrRe.MatchString(line):
		return "l-err"
	case logWarnRe.MatchString(line):
		return "l-warn"
	case logOKRe.MatchString(line):
		return "l-ok"
	case logInfoRe.MatchString(line):
		return "l-info"
	}
	return ""
}

// colorLog escapa el log y envuelve en <span class> las líneas que tienen color.
func colorLog(s string) template.HTML {
	s = ansiRe.ReplaceAllString(s, "")
	var b strings.Builder
	b.Grow(len(s) + len(s)/8)
	dockerfile := false // el Dockerfile generado se muestra tal cual, sin colores
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			b.WriteByte('\n')
		}
		esc := template.HTMLEscapeString(line)
		c := logClass(line)
		switch {
		case line == "Dockerfile generado:":
			dockerfile = true
		case c == "l-step":
			dockerfile = false
		case dockerfile:
			c = ""
		}
		if c != "" {
			b.WriteString(`<span class="` + c + `">` + esc + `</span>`)
		} else {
			b.WriteString(esc)
		}
	}
	return template.HTML(b.String())
}
