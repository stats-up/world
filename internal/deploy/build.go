package deploy

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/crypto/ssh"

	"world/internal/store"
)

// GenerateDeployKey crea un par ed25519 para clonar repos privados.
// La pública se agrega en GitHub → repo → Settings → Deploy keys (solo lectura).
func GenerateDeployKey(comment string) (privPEM []byte, pub string, err error) {
	pk, sk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	block, err := ssh.MarshalPrivateKey(sk, comment)
	if err != nil {
		return nil, "", err
	}
	sshPub, err := ssh.NewPublicKey(pk)
	if err != nil {
		return nil, "", err
	}
	pub = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " " + comment
	return pem.EncodeToMemory(block), pub, nil
}

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ParseEnv interpreta texto con formato .env y devuelve "CLAVE=valor".
// Acepta comentarios (#), líneas vacías, el prefijo "export " y valores entre comillas.
func ParseEnv(text string) ([]string, error) {
	var out []string
	var bad []string
	for i, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || !envKeyRe.MatchString(key) {
			bad = append(bad, fmt.Sprintf("línea %d", i+1))
			continue
		}
		val = strings.TrimSpace(val)
		if n := len(val); n >= 2 && (val[0] == '"' && val[n-1] == '"' || val[0] == '\'' && val[n-1] == '\'') {
			quote := val[0]
			val = val[1 : n-1]
			if quote == '"' {
				val = strings.NewReplacer(`\n`, "\n", `\"`, `"`, `\\`, `\`).Replace(val)
			}
		} else if j := strings.Index(val, " #"); j >= 0 {
			val = strings.TrimSpace(val[:j]) // comentario al final de la línea
		}
		out = append(out, key+"="+val)
	}
	if len(bad) > 0 {
		return out, fmt.Errorf("variables inválidas en: %s", strings.Join(bad, ", "))
	}
	return out, nil
}

// mergeEnv combina listas "CLAVE=valor"; las posteriores reemplazan a las anteriores.
func mergeEnv(lists ...[]string) []string {
	idx := map[string]int{}
	var out []string
	for _, list := range lists {
		for _, kv := range list {
			k, _, _ := strings.Cut(kv, "=")
			if i, ok := idx[k]; ok {
				out[i] = kv
				continue
			}
			idx[k] = len(out)
			out = append(out, kv)
		}
	}
	return out
}

const generatedDockerfile = "Dockerfile.world"

const defaultDockerignore = `.git
.env
node_modules
vendor
storage/logs/*
storage/framework/cache/*
storage/framework/sessions/*
storage/framework/views/*
`

// prepareBuild deja listo el contexto de build y devuelve el Dockerfile a usar.
func prepareBuild(site *store.Site, dir string, lg *logger) (string, error) {
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(dir, name))
		return err == nil
	}
	if site.Kind == store.KindDockerfile {
		if !exists("Dockerfile") {
			return "", errors.New("el sitio es de tipo 'dockerfile' pero el repo no tiene un Dockerfile en la raíz")
		}
		lg.Printf("Usando el Dockerfile del repositorio")
		return "Dockerfile", nil
	}
	if !exists(".dockerignore") {
		if err := os.WriteFile(filepath.Join(dir, ".dockerignore"), []byte(defaultDockerignore), 0o644); err != nil {
			return "", err
		}
	}
	var content string
	switch site.Kind {
	case store.KindStatic:
		content = "FROM nginx:1-alpine\nCOPY . /usr/share/nginx/html\n"
	case store.KindPHP:
		content = phpDockerfile(site, exists("composer.json"), exists("package.json"))
	default:
		return "", fmt.Errorf("tipo de sitio desconocido: %s", site.Kind)
	}
	lg.Printf("Dockerfile generado:\n%s", content)
	return generatedDockerfile, os.WriteFile(filepath.Join(dir, generatedDockerfile), []byte(content), 0o644)
}

// phpDockerfile usa las imágenes serversideup/php (nginx + PHP-FPM, pensadas para Laravel,
// corren sin root y escuchan en el puerto 8080).
func phpDockerfile(site *store.Site, hasComposer, hasPackageJSON bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "FROM serversideup/php:%s-fpm-nginx AS app\n", site.PHPVersion)
	if exts := strings.Fields(site.PHPExtensions); len(exts) > 0 {
		b.WriteString("USER root\n")
		fmt.Fprintf(&b, "RUN install-php-extensions %s\n", strings.Join(exts, " "))
		b.WriteString("USER www-data\n")
	}
	b.WriteString("WORKDIR /var/www/html\n")
	b.WriteString("COPY --chown=www-data:www-data . .\n")
	if hasComposer {
		// Al compilar no hay .env ni base de datos: si un ServiceProvider consulta la base al arrancar,
		// package:discover (script de composer) fallaría y tumbaría el build. Se ejecuta aparte y, si falla,
		// Laravel arma el manifiesto de paquetes solo en el primer arranque, ya con sus variables.
		b.WriteString("RUN composer install --no-dev --no-interaction --prefer-dist --optimize-autoloader --no-scripts \\\n" +
			"    && (composer run-script post-autoload-dump --no-dev --no-interaction \\\n" +
			"        || echo '⚠ world: los scripts de composer fallaron sin .env/base de datos; Laravel descubrirá los paquetes al iniciar')\n")
	}
	if hasPackageJSON && site.BuildAssets {
		// Los assets se compilan después de composer: Livewire/Flux importan CSS desde vendor/.
		b.WriteString("\nFROM node:22-alpine AS assets\n")
		b.WriteString("WORKDIR /app\n")
		b.WriteString("COPY --from=app /var/www/html /app\n")
		b.WriteString("RUN if [ -f package-lock.json ]; then npm ci; else npm install; fi && npm run build\n")
		b.WriteString("\nFROM app\n")
		b.WriteString("COPY --from=assets --chown=www-data:www-data /app/public /var/www/html/public\n")
	}
	return b.String()
}
