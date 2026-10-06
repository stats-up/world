package mail

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

func selfSigned(t *testing.T) tls.Certificate {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"otro.example"}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// fakeIMAP atiende una conexión al estilo Dovecot con STARTTLS (puerto distinto de 993).
func fakeIMAP(t *testing.T, password string) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	cert := selfSigned(t)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				var conn net.Conn = c
				r := bufio.NewReader(conn)
				send := func(s string) { conn.Write([]byte(s + "\r\n")) }
				send("* OK [CAPABILITY IMAP4rev1 STARTTLS AUTH=PLAIN] Dovecot ready.")
				for {
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					tag, cmd, _ := strings.Cut(strings.TrimRight(l, "\r\n"), " ")
					switch {
					case cmd == "STARTTLS":
						send(tag + " OK Begin TLS")
						tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}})
						conn, r = tc, bufio.NewReader(tc)
					case cmd == "AUTHENTICATE PLAIN":
						send("+ ")
						resp, _ := r.ReadString('\n')
						b, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(resp))
						if string(b) == "\x00fer@statsup.cl\x00"+password {
							send(tag + " OK Logged in")
						} else {
							send(tag + " NO [AUTHENTICATIONFAILED] Authentication failed.")
						}
					case strings.HasPrefix(cmd, "LOGIN"):
						send(tag + " NO [AUTHENTICATIONFAILED] Authentication failed.")
					case cmd == `LIST "" "*"`:
						send(`* LIST (\HasNoChildren) "." INBOX`)
						send(`* LIST (\HasNoChildren \Sent) "." "INBOX.Sent"`)
						send(`* LIST (\Noselect \HasChildren) "." "Archivo"`)
						send(`* LIST (\HasNoChildren) "." {11}`)
						conn.Write([]byte("Año pasado"))
						send("")
						send(tag + " OK List completed")
					case strings.HasPrefix(cmd, "STATUS "):
						n := "100"
						if strings.Contains(cmd, "Sent") {
							n = "23"
						}
						send("* STATUS x (MESSAGES " + n + ")")
						send(tag + " OK Status completed")
					case cmd == "LOGOUT":
						send("* BYE")
						send(tag + " OK")
						return
					default:
						send(tag + " BAD")
					}
				}
			}(c)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestProbeSource(t *testing.T) {
	port := fakeIMAP(t, `cl@ve "rara"\ñ`)
	ctx := context.Background()
	info, err := ProbeSource(ctx, "127.0.0.1", port, "fer@statsup.cl", `cl@ve "rara"\ñ`)
	if err != nil {
		t.Fatal(err)
	}
	if info.Folders != 3 || info.Messages != 223 || info.Server != "Dovecot ready." {
		t.Fatalf("info: %+v", info)
	}
	if _, err := ProbeSource(ctx, "127.0.0.1", port, "fer@statsup.cl", "mala"); !errors.Is(err, ErrSourceAuth) {
		t.Fatalf("contraseña mala: %v", err)
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	if _, err := ProbeSource(ctx, "127.0.0.1", closed, "u", "p"); err == nil || !strings.Contains(err.Error(), "no se pudo conectar") {
		t.Fatalf("puerto cerrado: %v", err)
	}
}
