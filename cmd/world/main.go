// world: panel liviano para administrar sitios web en contenedores Docker
// con dominios, subdominios y SSL automático (Traefik + Let's Encrypt).
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"world/internal/auth"
	"world/internal/config"
	"world/internal/deploy"
	"world/internal/docker"
	"world/internal/mail"
	"world/internal/monitor"
	"world/internal/proxy"
	"world/internal/secret"
	"world/internal/store"
	"world/internal/web"
)

// version se inyecta al compilar: -ldflags "-X main.version=..."
var version = "dev"

const usage = `Uso: world <comando>

Comandos:
  serve                      inicia el panel (por defecto)
  version                    muestra la versión
  reset-password <email>     genera una contraseña nueva para el usuario
  disable-2fa <email>        desactiva el 2FA del usuario (si perdió el teléfono)
`

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	cfg := config.Load()
	var err error
	switch cmd {
	case "serve":
		err = serve(cfg)
	case "version", "--version", "-v":
		fmt.Println(version)
	case "reset-password", "disable-2fa":
		if len(os.Args) < 3 {
			fmt.Fprint(os.Stderr, usage)
			os.Exit(2)
		}
		err = userCommand(cfg, cmd, os.Args[2])
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func serve(cfg config.Config) error {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	box, err := secret.LoadOrCreate(cfg.Path("secret.key"))
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.Path("world.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	st.FailInterrupted()
	st.PurgeExpiredSessions()

	if n, _ := st.CountUsers(); n == 0 {
		token, err := setupToken(cfg)
		if err != nil {
			return err
		}
		log.Printf("Sin usuarios todavía. Abre el panel y usa el código de instalación: %s", token)
	}

	dc := docker.New(cfg.DockerSocket)
	px := proxy.New(dc, cfg)
	mx := mail.New(dc, cfg)
	engine := deploy.New(cfg, st, dc, box)
	poller := deploy.NewPoller(engine, st, time.Minute)
	cron := deploy.NewCron(engine, st, time.Minute)
	mon := monitor.New(dc, st)
	srv, err := web.New(cfg, st, dc, box, engine, poller, cron, px, mx, mon, version)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go poller.Run(ctx)
	go cron.Run(ctx)
	go px.EnsureLoop(ctx, func() (string, string) {
		return st.Setting("acme_email"), st.Setting("panel_domain")
	})
	go mon.Run(ctx)
	go mx.EnsureLoop(ctx, func() mail.Settings { return mail.LoadSettings(st, box) })

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutdownCtx)
	}()

	log.Printf("world %s escuchando en %s (datos en %s)", version, cfg.Listen, cfg.DataDir)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// setupToken devuelve el código de instalación, creándolo si no existe.
// Evita que otra persona cree el administrador si encuentra el panel antes que tú.
func setupToken(cfg config.Config) (string, error) {
	path := cfg.Path("setup-token")
	if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
		return string(b), nil
	}
	token := auth.RandomToken(12)
	return token, os.WriteFile(path, []byte(token), 0o600)
}

func userCommand(cfg config.Config, cmd, email string) error {
	st, err := store.Open(cfg.Path("world.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	user, err := st.UserByEmail(email)
	if err != nil {
		return fmt.Errorf("usuario %s: %w", email, err)
	}
	switch cmd {
	case "reset-password":
		pw := auth.RandomToken(12)
		hash, err := auth.HashPassword(pw)
		if err != nil {
			return err
		}
		if err := st.SetPassword(user.ID, hash); err != nil {
			return err
		}
		st.DeleteUserSessions(user.ID, "")
		fmt.Printf("Nueva contraseña para %s: %s\nCámbiala desde tu perfil al ingresar.\n", email, pw)
	case "disable-2fa":
		if err := st.SetTOTP(user.ID, "", false); err != nil {
			return err
		}
		fmt.Printf("2FA desactivado para %s.\n", email)
	}
	return nil
}
