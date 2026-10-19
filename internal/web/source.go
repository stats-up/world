package web

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"world/internal/proxy"
)

// El panel corre en el host y escucha en todas las interfaces (la instalación inicial se hace por
// IP:puerto), así que cualquier contenedor puede llegar a él por el gateway de su red sin pasar por
// Traefik. sourceGuard rechaza esas conexiones internas, salvo la de Traefik, y es el único lugar
// donde se confía en X-Forwarded-For: solo si la conexión viene de Traefik.

type clientIPKey struct{}

// trustedProxy recuerda las IPs del contenedor de Traefik. Si una IP privada no está en la lista se
// vuelve a consultar Docker (con un mínimo entre consultas), por si Traefik se recreó y cambió de IP.
type trustedProxy struct {
	lookup func(ctx context.Context) ([]net.IP, error)

	mu      sync.Mutex
	ips     []net.IP
	checked time.Time
}

const proxyRecheck = 5 * time.Second

func (t *trustedProxy) is(ip net.IP) bool {
	t.mu.Lock()
	if containsIP(t.ips, ip) {
		t.mu.Unlock()
		return true
	}
	if t.lookup == nil || time.Since(t.checked) < proxyRecheck {
		t.mu.Unlock()
		return false
	}
	t.checked = time.Now()
	t.mu.Unlock()

	// Fuera del lock: mientras se consulta Docker, las peticiones de Traefik siguen pasando.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ips, err := t.lookup(ctx)
	if err != nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ips = ips
	return containsIP(ips, ip)
}

func containsIP(list []net.IP, ip net.IP) bool {
	for _, p := range list {
		if p.Equal(ip) {
			return true
		}
	}
	return false
}

// traefikIPs devuelve las IPs que tiene el contenedor de Traefik en sus redes.
func (s *Server) traefikIPs(ctx context.Context) ([]net.IP, error) {
	info, err := s.dc.ContainerInspect(ctx, proxy.ContainerName)
	if err != nil {
		return nil, err
	}
	var ips []net.IP
	for _, n := range info.NetworkSettings.Networks {
		if ip := net.ParseIP(n.IPAddress); ip != nil {
			ips = append(ips, ip)
		}
	}
	return ips, nil
}

// internalIP: direcciones de redes privadas (Docker, la VPC de Lightsail) o de enlace local.
func internalIP(ip net.IP) bool {
	return ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
}

func sourceGuard(tp *trustedProxy, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		ip := net.ParseIP(host)
		client := host
		switch {
		case ip == nil, ip.IsLoopback():
			// Local (auto-update, curl en el servidor): se usa la conexión tal cual.
		case tp.is(ip):
			// Traefik agrega al final la IP de quien se conectó a él.
			if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
				parts := strings.Split(xff, ",")
				if real := net.ParseIP(strings.TrimSpace(parts[len(parts)-1])); real != nil {
					client = real.String()
				}
			}
		case internalIP(ip):
			http.Error(w, "Acceso directo al panel bloqueado: entra por su dominio (HTTPS).", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientIPKey{}, client)))
	})
}

func clientIP(r *http.Request) string {
	if ip, ok := r.Context().Value(clientIPKey{}).(string); ok {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
