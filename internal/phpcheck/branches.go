package phpcheck

import (
	"fmt"
	"time"
)

// Branch es una rama de PHP que world ofrece, con la fecha hasta la que recibe parches de seguridad
// (php.net/supported-versions: cada rama tiene 2 años de soporte activo + 2 de solo seguridad, hasta
// el 31 de diciembre). Al salir una rama nueva se agrega aquí (y serversideup/php debe tener su imagen).
type Branch struct {
	Version       string
	SecurityUntil time.Time
}

func eoy(year int) time.Time { return time.Date(year, 12, 31, 23, 59, 59, 0, time.UTC) }

// Branches: de la más nueva a la más antigua.
var Branches = []Branch{
	{"8.5", eoy(2029)},
	{"8.4", eoy(2028)},
	{"8.3", eoy(2027)},
	{"8.2", eoy(2026)},
	{"8.1", eoy(2025)},
}

// DefaultBranch: la que se propone para sitios nuevos (la más nueva con amplio soporte de Laravel).
const DefaultBranch = "8.4"

// EOLWarning: con cuánta anticipación se avisa que una rama deja de recibir parches.
const EOLWarning = 120 * 24 * time.Hour

func BranchInfo(v string) (Branch, bool) {
	for _, b := range Branches {
		if b.Version == v {
			return b, true
		}
	}
	return Branch{}, false
}

func Versions() []string {
	out := make([]string, len(Branches))
	for i, b := range Branches {
		out[i] = b.Version
	}
	return out
}

func (b Branch) EOL(now time.Time) bool { return now.After(b.SecurityUntil) }

// EndingSoon: todavía recibe parches, pero deja de hacerlo dentro de EOLWarning.
func (b Branch) EndingSoon(now time.Time) bool {
	return !b.EOL(now) && b.SecurityUntil.Sub(now) < EOLWarning
}

var meses = [...]string{"ene", "feb", "mar", "abr", "may", "jun", "jul", "ago", "sept", "oct", "nov", "dic"}

// Label: texto para el selector, ej. "8.3 · parches hasta dic 2027" o "8.1 · sin soporte desde dic 2025".
func (b Branch) Label(now time.Time) string {
	m := fmt.Sprintf("%s %d", meses[b.SecurityUntil.Month()-1], b.SecurityUntil.Year())
	if b.EOL(now) {
		return b.Version + " · sin parches desde " + m
	}
	return b.Version + " · parches hasta " + m
}
