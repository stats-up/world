package mail

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"world/internal/secret"
)

type memStore map[string]string

func (m memStore) Setting(k string) string      { return m[k] }
func (m memStore) SetSetting(k, v string) error { m[k] = v; return nil }

func TestSettingsRoundTrip(t *testing.T) {
	box, err := secret.LoadOrCreate(filepath.Join(t.TempDir(), "k"))
	if err != nil {
		t.Fatal(err)
	}
	st := memStore{}
	in := Settings{Enabled: true, Hostname: "mx.statsup.cl", S3Bucket: "statsup", S3Prefix: "mail/",
		S3AccessKey: "AK", S3SecretKey: "SK", CFToken: "CF", AdminSecret: "admin-pass"}
	if err := SaveSettings(st, box, in); err != nil {
		t.Fatal(err)
	}
	for k, v := range st {
		if strings.HasSuffix(k, "_enc") && (strings.Contains(v, "SK") || strings.Contains(v, "admin-pass")) {
			t.Fatalf("%s quedó sin cifrar", k)
		}
	}
	if got := LoadSettings(st, box); got != in {
		t.Fatalf("got %+v, want %+v", got, in)
	}

	// Secretos vacíos en el formulario = conservar los actuales.
	in2 := in
	in2.S3SecretKey, in2.CFToken, in2.AdminSecret, in2.S3AccessKey = "", "", "", ""
	in2.Enabled = false
	if err := SaveSettings(st, box, in2); err != nil {
		t.Fatal(err)
	}
	got := LoadSettings(st, box)
	if got.Enabled || got.S3SecretKey != "SK" || got.AdminSecret != "admin-pass" {
		t.Fatalf("no conservó los secretos: %+v", got)
	}
}

func TestLoadDefaults(t *testing.T) {
	box, _ := secret.LoadOrCreate(filepath.Join(t.TempDir(), "k"))
	s := LoadSettings(memStore{}, box)
	if s.Enabled || s.Hostname != "mx.statsup.cl" || s.S3Prefix != "mail/" {
		t.Fatalf("defaults: %+v", s)
	}
	if len(s.Missing()) != 3 { // el nombre del servidor tiene valor por defecto
		t.Fatalf("missing: %v", s.Missing())
	}
}

func TestEnv(t *testing.T) {
	s := Settings{Hostname: "mx.statsup.cl", S3AccessKey: "AK", S3SecretKey: "SK", CFToken: "CF", AdminSecret: "pw:con:dos-puntos"}
	env := s.env()
	for _, want := range []string{"STALWART_HOSTNAME=mx.statsup.cl", "STALWART_RECOVERY_ADMIN=admin:pw:con:dos-puntos",
		"S3_ACCESS_KEY=AK", "S3_SECRET_KEY=SK", "CF_API_TOKEN=CF"} {
		if !slices.Contains(env, want) {
			t.Errorf("falta %q en %v", want, env)
		}
	}
	if !slices.IsSorted(env) {
		t.Error("env debe ir ordenado para que el hash sea estable")
	}
}

func TestStatusOK(t *testing.T) {
	cases := map[Status]bool{
		{State: "running", Health: "healthy"}:   true,
		{State: "running", Health: "starting"}:  true,
		{State: "running", Health: "unhealthy"}: false,
		{State: "exited"}:                       false,
		{State: "no instalado"}:                 false,
	}
	for st, want := range cases {
		if st.OK() != want {
			t.Errorf("%+v: OK()=%v", st, !want)
		}
	}
}
