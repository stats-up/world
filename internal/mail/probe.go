package mail

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Probar la conexión al servidor de origen antes de importar: inicia sesión por IMAP
// y cuenta carpetas y correos. Es de solo lectura (no abre ni marca ningún correo).

// SourceInfo es lo que se encontró en el buzón de origen.
type SourceInfo struct {
	Folders  int
	Messages int64
	Server   string // saludo del servidor (ej: "Dovecot ready.")
	Partial  bool   // había demasiadas carpetas: solo se contaron las primeras
}

// ErrSourceAuth: el origen rechazó el usuario o la contraseña.
var ErrSourceAuth = errors.New("usuario o contraseña de origen incorrectos")

const maxProbeFolders = 1000

var (
	listRe   = regexp.MustCompile(`^\* LIST \(([^)]*)\) (?:"[^"]*"|NIL) (.+)$`)
	statusRe = regexp.MustCompile(`^\* STATUS .*\(MESSAGES (\d+)\)`)
)

type imapConn struct {
	c   net.Conn
	r   *bufio.Reader
	tag int
}

func (ic *imapConn) readLine() (string, error) {
	l, err := ic.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	l = strings.TrimRight(l, "\r\n")
	// Literal {n}: se lee y se agrega a la línea (nombres de carpeta raros).
	for strings.HasSuffix(l, "}") {
		i := strings.LastIndexByte(l, '{')
		n, err := strconv.Atoi(strings.TrimSuffix(l[i+1:], "}"))
		if i < 0 || err != nil || n > 1<<16 {
			break
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(ic.r, buf); err != nil {
			return "", err
		}
		rest, err := ic.r.ReadString('\n')
		if err != nil {
			return "", err
		}
		l = l[:i] + strconv.Quote(string(buf)) + strings.TrimRight(rest, "\r\n")
	}
	return l, nil
}

// cmd envía un comando y devuelve las respuestas no etiquetadas y el resultado (OK/NO/BAD + texto).
func (ic *imapConn) cmd(format string, args ...any) ([]string, string, error) {
	ic.tag++
	tag := fmt.Sprintf("w%d", ic.tag)
	if _, err := fmt.Fprintf(ic.c, tag+" "+format+"\r\n", args...); err != nil {
		return nil, "", err
	}
	var untagged []string
	for {
		l, err := ic.readLine()
		if err != nil {
			return nil, "", err
		}
		if strings.HasPrefix(l, tag+" ") {
			return untagged, strings.TrimPrefix(l, tag+" "), nil
		}
		if strings.HasPrefix(l, "+") {
			return untagged, l, nil // continuación (AUTHENTICATE sin SASL-IR)
		}
		untagged = append(untagged, l)
	}
}

func imapQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// ProbeSource se conecta al origen (993 SSL o 143 STARTTLS), inicia sesión y cuenta los correos.
func ProbeSource(ctx context.Context, host string, port int, user, password string) (*SourceInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	// Igual que imapsync: no se verifica el certificado (los cPanel suelen tener uno de otro nombre).
	tlsCfg := &tls.Config{ServerName: host, InsecureSkipVerify: true} //nolint:gosec
	d := &net.Dialer{Timeout: 15 * time.Second}
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("no se pudo conectar a %s: %w", addr, unwrapNet(err))
	}
	defer raw.Close()
	if dl, ok := ctx.Deadline(); ok {
		raw.SetDeadline(dl)
	}
	conn := raw
	if port == 993 {
		tc := tls.Client(raw, tlsCfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			return nil, fmt.Errorf("falló la conexión segura (SSL) con %s: %w", addr, err)
		}
		conn = tc
	}
	ic := &imapConn{c: conn, r: bufio.NewReader(conn)}
	greeting, err := ic.readLine()
	if err != nil {
		return nil, fmt.Errorf("el servidor no respondió como IMAP: %w", err)
	}
	if !strings.HasPrefix(greeting, "* OK") {
		return nil, fmt.Errorf("el servidor no respondió como IMAP: %s", greeting)
	}
	info := &SourceInfo{Server: strings.TrimSpace(strings.TrimPrefix(greeting, "* OK"))}
	if i := strings.Index(info.Server, "] "); strings.HasPrefix(info.Server, "[") && i > 0 {
		info.Server = info.Server[i+2:]
	}
	if port != 993 {
		if _, res, err := ic.cmd("STARTTLS"); err != nil || !strings.HasPrefix(res, "OK") {
			return nil, fmt.Errorf("el servidor no acepta STARTTLS en el puerto %d: %s", port, res)
		}
		tc := tls.Client(raw, tlsCfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			return nil, fmt.Errorf("falló STARTTLS con %s: %w", addr, err)
		}
		ic = &imapConn{c: tc, r: bufio.NewReader(tc), tag: ic.tag}
	}

	// AUTHENTICATE PLAIN admite cualquier carácter en la contraseña (LOGIN no siempre).
	plain := base64.StdEncoding.EncodeToString([]byte("\x00" + user + "\x00" + password))
	_, res, err := ic.cmd("AUTHENTICATE PLAIN")
	if err == nil && strings.HasPrefix(res, "+") {
		_, err = fmt.Fprintf(ic.c, "%s\r\n", plain)
		if err == nil {
			for {
				var l string
				if l, err = ic.readLine(); err != nil || strings.HasPrefix(l, fmt.Sprintf("w%d ", ic.tag)) {
					res = strings.TrimPrefix(l, fmt.Sprintf("w%d ", ic.tag))
					break
				}
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("se cortó la conexión al iniciar sesión: %w", err)
	}
	if !strings.HasPrefix(res, "OK") {
		// Algunos servidores no tienen AUTH=PLAIN: se intenta con LOGIN.
		if _, res, err = ic.cmd("LOGIN %s %s", imapQuote(user), imapQuote(password)); err != nil {
			return nil, fmt.Errorf("se cortó la conexión al iniciar sesión: %w", err)
		}
		if !strings.HasPrefix(res, "OK") {
			return nil, ErrSourceAuth
		}
	}

	lines, res, err := ic.cmd(`LIST "" "*"`)
	if err != nil || !strings.HasPrefix(res, "OK") {
		return nil, fmt.Errorf("no se pudieron listar las carpetas: %v %s", err, res)
	}
	var folders []string
	for _, l := range lines {
		m := listRe.FindStringSubmatch(l)
		if m == nil || strings.Contains(strings.ToLower(m[1]), `\noselect`) || strings.Contains(strings.ToLower(m[1]), `\nonexistent`) {
			continue
		}
		folders = append(folders, m[2])
	}
	info.Folders = len(folders)
	if len(folders) > maxProbeFolders {
		folders, info.Partial = folders[:maxProbeFolders], true
	}
	for _, f := range folders {
		lines, _, err := ic.cmd("STATUS %s (MESSAGES)", f)
		if err != nil {
			return nil, fmt.Errorf("contando correos: %w", err)
		}
		for _, l := range lines {
			if m := statusRe.FindStringSubmatch(l); m != nil {
				n, _ := strconv.ParseInt(m[1], 10, 64)
				info.Messages += n
			}
		}
	}
	ic.cmd("LOGOUT")
	return info, nil
}

func unwrapNet(err error) error {
	var op *net.OpError
	if errors.As(err, &op) && op.Err != nil {
		return op.Err
	}
	return err
}
