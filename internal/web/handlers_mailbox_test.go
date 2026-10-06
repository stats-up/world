package web

import (
	"strings"
	"testing"
)

func TestParseAliases(t *testing.T) {
	got, err := parseAliases("Contacto, info@otro.cl  ventas;info\nventas", "ventas")
	if err != nil || strings.Join(got, ",") != "contacto,info" {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := parseAliases("hola mundo!", ""); err == nil {
		t.Fatal("debió rechazar 'mundo!'")
	}
}

func TestParseQuotaGB(t *testing.T) {
	for in, want := range map[string]int64{"": 0, "0": 0, "5": 5 << 30, "1,5": 3 << 29, " 20 ": 20 << 30} {
		got, err := parseQuotaGB(in)
		if err != nil || got != want {
			t.Errorf("%q: %d %v", in, got, err)
		}
	}
	for _, in := range []string{"-1", "abc", "99999"} {
		if _, err := parseQuotaGB(in); err == nil {
			t.Errorf("%q debió fallar", in)
		}
	}
}

func TestGeneratePassword(t *testing.T) {
	a, b := generatePassword(), generatePassword()
	if len(a) != 20 || a == b || strings.ContainsAny(a, "0O1lI") {
		t.Fatalf("%q %q", a, b)
	}
}
