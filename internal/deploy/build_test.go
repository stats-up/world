package deploy

import (
	"reflect"
	"strings"
	"testing"

	"world/internal/store"
)

func TestParseEnv(t *testing.T) {
	in := "# comentario\nAPP_NAME=\"Mi App\"\nexport APP_ENV=production\n\nDB_PASSWORD='p#ss'\nMAIL_HOST=smtp.x.cl # comentario\nURL=https://a.cl/?x=1\n"
	got, err := ParseEnv(in)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"APP_NAME=Mi App", "APP_ENV=production", "DB_PASSWORD=p#ss", "MAIL_HOST=smtp.x.cl", "URL=https://a.cl/?x=1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if _, err := ParseEnv("SIN_IGUAL\n1MAL=x"); err == nil {
		t.Fatal("se esperaba error con líneas inválidas")
	}
}

func TestMergeEnv(t *testing.T) {
	got := mergeEnv([]string{"A=1", "B=2"}, []string{"B=3", "C=4"})
	if !reflect.DeepEqual(got, []string{"A=1", "B=3", "C=4"}) {
		t.Fatalf("got %q", got)
	}
}

func TestPHPDockerfile(t *testing.T) {
	site := &store.Site{PHPVersion: "8.3", PHPExtensions: "intl gd", BuildAssets: true}
	df := phpDockerfile(site, true, true)
	for _, s := range []string{"serversideup/php:8.3-fpm-nginx", "install-php-extensions intl gd", "composer install", "npm run build", "FROM app"} {
		if !strings.Contains(df, s) {
			t.Errorf("falta %q en:\n%s", s, df)
		}
	}
	if df := phpDockerfile(&store.Site{PHPVersion: "8.4"}, false, true); strings.Contains(df, "npm") || strings.Contains(df, "USER root") {
		t.Errorf("no debería compilar assets ni instalar extensiones:\n%s", df)
	}
}

func TestDeployKey(t *testing.T) {
	priv, pub, err := GenerateDeployKey("world-test")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(pub, "ssh-ed25519 ") || !strings.Contains(string(priv), "OPENSSH PRIVATE KEY") {
		t.Fatalf("llaves con formato inesperado: %s", pub)
	}
}
