package backup

import (
	"strings"
	"testing"
	"time"

	"world/internal/store"
)

func TestTargetOf(t *testing.T) {
	tg, err := TargetOf([]string{"DB_CONNECTION=mysql", "DB_HOST=db.example.com", "DB_PORT=3307", "DB_DATABASE=tienda",
		"DB_USERNAME=app", "DB_PASSWORD=p=a\"s"})
	if err != nil || tg.Host != "db.example.com" || tg.Port != 3307 || tg.Database != "tienda" || tg.Password != `p=a"s` {
		t.Fatalf("%+v %v", tg, err)
	}
	if tg, _ := TargetOf([]string{"DB_HOST=h", "DB_DATABASE=d", "DB_USERNAME=u"}); tg.Port != 3306 {
		t.Errorf("puerto por defecto: %d", tg.Port)
	}
	for env, want := range map[string]string{
		"DB_CONNECTION=sqlite": "solo se respaldan",
		"DB_HOST=h":            "DB_DATABASE, DB_USERNAME",
		"DB_HOST=localhost,DB_DATABASE=d,DB_USERNAME=u":   "localhost",
		"DB_HOST=h,DB_DATABASE=d;drop,DB_USERNAME=u":      "inválido",
		"DB_HOST=h,DB_DATABASE=d,DB_USERNAME=u,DB_PORT=x": "DB_PORT",
	} {
		if _, err := TargetOf(strings.Split(env, ",")); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v (quería %q)", env, err, want)
		}
	}
}

func TestLastLines(t *testing.T) {
	logs := "2026-11-27T06:00:01.1Z mysqldump: [Warning] Using a password\n" +
		"2026-11-27T06:00:02.2Z mysqldump: Got error: 1045: Access denied for user 'app'\n\n"
	if got := lastLines(logs, 1); got != "mysqldump: Got error: 1045: Access denied for user 'app'" {
		t.Errorf("%q", got)
	}
	if got := lastLines("", 3); !strings.Contains(got, "sin mensajes") {
		t.Errorf("%q", got)
	}
}

func TestDue(t *testing.T) {
	loc, _ := time.LoadLocation("America/Santiago")
	c := store.DefaultBackupConfig(1)
	c.Enabled, c.Days, c.Hours = true, "12345", []int{3, 15}
	thu3 := time.Date(2026, 11, 26, 3, 10, 0, 0, loc) // jueves
	sat3 := time.Date(2026, 11, 28, 3, 10, 0, 0, loc) // sábado
	if !c.Due(thu3) || c.Due(thu3.Add(time.Hour)) || c.Due(sat3) || !c.Due(thu3.Add(12*time.Hour)) {
		t.Error("horario")
	}
	c.Enabled = false
	if c.Due(thu3) {
		t.Error("desactivado no corre")
	}
	if h, err := store.ParseBackupHours("15, 03:00 3,23h"); err != nil || len(h) != 3 || h[0] != 3 || h[2] != 23 {
		t.Errorf("%v %v", h, err)
	}
	for _, bad := range []string{"", "24", "tarde"} {
		if _, err := store.ParseBackupHours(bad); err == nil {
			t.Errorf("%q debió fallar", bad)
		}
	}
}
