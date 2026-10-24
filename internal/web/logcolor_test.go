package web

import (
	"strings"
	"testing"
)

func TestLogClass(t *testing.T) {
	cases := map[string]string{
		"==> Construyendo imagen world/x:1":                        "l-step",
		"--- Salida del contenedor al iniciar ---":                 "l-step",
		"✓ Deploy completado en 29s":                               "l-ok",
		"Health check OK":                                          "l-ok",
		"#9 DONE 0.2s":                                             "l-ok",
		"   INFO  Nothing to migrate.":                             "l-ok",
		"🚀 Running migrations for default database":                "l-info",
		"   INFO  Running migrations.":                             "l-info",
		"⚠ world: los scripts de composer fallaron":                "l-warn",
		"npm warn deprecated inflight@1.0.6":                       "l-warn",
		"[2026-10-24] production.WARNING: lento":                   "l-warn",
		"✗ Deploy fallido: el contenedor se detuvo al arrancar":    "l-err",
		"SQLSTATE[42S01]: Base table or view already exists ERROR": "l-err",
		`1.2.3.4 - - "GET / HTTP/1.1" 500 12 "-"`:                  "l-err",
		`1.2.3.4 - - "GET /x HTTP/1.1" 404 12 "-"`:                 "l-warn",
		`1.2.3.4 - - "GET / HTTP/1.1" 200 12 "-"`:                  "",
		"error_reporting = E_ALL":                                  "",
		"#6 CACHED":                                                "",
	}
	for line, want := range cases {
		if got := logClass(line); got != want {
			t.Errorf("%q: %q, quería %q", line, got, want)
		}
	}
}

func TestColorLogEscapes(t *testing.T) {
	got := string(colorLog("\x1b[31m<b>error</b>\x1b[0m\nok <i>"))
	want := `<span class="l-err">&lt;b&gt;error&lt;/b&gt;</span>` + "\n" + "ok &lt;i&gt;"
	if got != want {
		t.Fatalf("got %q", got)
	}
	if strings.Contains(got, "\x1b") {
		t.Error("quedaron códigos ANSI")
	}
}
