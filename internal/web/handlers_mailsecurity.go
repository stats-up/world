package web

import (
	"context"
	"net"
	"net/http"
	"strings"

	"world/internal/mail"
	"world/internal/proxy"
)

// Correo → Seguridad: IPs bloqueadas por Stalwart (se pueden desbloquear) e IPs permitidas
// (nunca se bloquean automáticamente).

// serverAddr es una dirección del propio servidor que Stalwart no debería bloquear.
type serverAddr struct {
	Address string // IP o red
	Label   string
	Allowed bool // ya está cubierta por una IP permitida
}

// blockedRow es una IP bloqueada en la tabla, marcada si es del propio servidor.
type blockedRow struct {
	mail.IPEntry
	Server string // descripción si es una dirección del servidor
}

// serverAddrs: las IPs públicas del nombre del servidor (los sitios llegaban por ahí)
// y la red interna de Docker (world, los sitios y Stalwart).
func (s *Server) serverAddrs(ctx context.Context) []serverAddr {
	var out []serverAddr
	if host := s.serverHostname(); host != "" {
		if ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", host); err == nil {
			for _, ip := range ips {
				out = append(out, serverAddr{Address: ip.String(), Label: "IP pública del servidor (" + host + ")"})
			}
		}
	}
	if subnets, err := s.dc.NetworkSubnets(ctx, proxy.Network); err == nil {
		for _, n := range subnets {
			out = append(out, serverAddr{Address: n, Label: "Red interna de Docker (world, sitios y Stalwart)"})
		}
	}
	return out
}

// serverLabel dice si la IP bloqueada es del servidor (o cae en su red interna).
func serverLabel(addr string, server []serverAddr) string {
	ip := addr
	if i := strings.IndexByte(ip, '/'); i >= 0 {
		ip = ip[:i]
	}
	for _, sa := range server {
		if mail.Covers(sa.Address, ip) {
			return sa.Label
		}
	}
	return ""
}

func (s *Server) mailSecurity(w http.ResponseWriter, r *http.Request) {
	if !mail.Enabled(s.st) {
		s.render(w, r, "mail", data{"Disabled": true})
		return
	}
	ctx, cancel := s.mailCtx(r)
	defer cancel()
	blocked, err := s.mailAPI.BlockedIPs(ctx)
	if err != nil {
		s.mailError(w, r, err)
		return
	}
	allowed, err := s.mailAPI.AllowedIPs(ctx)
	if err != nil {
		s.mailError(w, r, err)
		return
	}
	server := s.serverAddrs(ctx)
	for i := range server {
		for _, a := range allowed {
			if a.Address == server[i].Address || mail.Covers(a.Address, strings.SplitN(server[i].Address, "/", 2)[0]) {
				server[i].Allowed = true
			}
		}
	}
	rows := make([]blockedRow, len(blocked))
	var serverBlocked []blockedRow
	for i, b := range blocked {
		rows[i] = blockedRow{IPEntry: b, Server: serverLabel(b.Address, server)}
		if rows[i].Server != "" {
			serverBlocked = append(serverBlocked, rows[i])
		}
	}
	s.render(w, r, "mail_security", data{"Blocked": rows, "ServerBlocked": serverBlocked, "Allowed": allowed,
		"ServerAddrs": server, "Form": map[string]string{}})
}

func (s *Server) mailSecurityUnblock(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	ids := r.Form["id"]
	if len(ids) == 0 {
		s.mailBack(w, r, "/mail/security", "error", "No se eligió ninguna IP.")
		return
	}
	ctx, cancel := s.mailCtx(r)
	defer cancel()
	if err := s.mailAPI.Unblock(ctx, ids...); err != nil {
		s.mailBack(w, r, "/mail/security", "error", "No se pudo desbloquear: "+err.Error())
		return
	}
	msg := "IP desbloqueada."
	if len(ids) > 1 {
		msg = "IPs desbloqueadas."
	}
	s.mailBack(w, r, "/mail/security", "ok", msg+" Ya puede volver a conectarse.")
}

func (s *Server) mailSecurityAllow(w http.ResponseWriter, r *http.Request) {
	addr, err := mail.NormalizeIPMask(r.FormValue("address"))
	if err != nil {
		s.mailBack(w, r, "/mail/security#permitidas", "error", err.Error())
		return
	}
	ctx, cancel := s.mailCtx(r)
	defer cancel()
	if err := s.mailAPI.Allow(ctx, addr, r.FormValue("reason")); err != nil {
		s.mailBack(w, r, "/mail/security#permitidas", "error", "No se pudo agregar: "+err.Error())
		return
	}
	s.mailBack(w, r, "/mail/security#permitidas", "ok", addr+" quedó permitida: Stalwart no la bloqueará automáticamente.")
}

func (s *Server) mailSecurityRemoveAllowed(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.mailCtx(r)
	defer cancel()
	if err := s.mailAPI.RemoveAllowed(ctx, r.PathValue("id")); err != nil {
		s.mailBack(w, r, "/mail/security#permitidas", "error", "No se pudo quitar: "+err.Error())
		return
	}
	s.mailBack(w, r, "/mail/security#permitidas", "ok", "IP quitada de las permitidas.")
}
