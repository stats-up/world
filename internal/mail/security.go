package mail

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

// Seguridad de Stalwart: IPs bloqueadas (a mano o por el bloqueo automático ante fallas de
// autenticación, escaneos, etc.) e IPs permitidas, que nunca se bloquean automáticamente.
//
// Probado contra v0.16.25: los cambios no se aplican a las conexiones hasta recargar.
// Desbloquear necesita la acción ReloadBlockedIps; las permitidas, ReloadSettings. Una IP
// permitida no levanta un bloqueo que ya existe: hay que desbloquearla también.

// IPEntry es una IP o red bloqueada o permitida.
type IPEntry struct {
	ID        string     `json:"id"`
	Address   string     `json:"address"`
	Reason    *string    `json:"reason"`
	CreatedAt time.Time  `json:"createdAt"`
	ExpiresAt *time.Time `json:"expiresAt"`
}

func (e IPEntry) ReasonText() string {
	if e.Reason == nil {
		return ""
	}
	return *e.Reason
}

// BlockReason traduce el motivo de un bloqueo.
func (e IPEntry) BlockReason() string {
	switch e.ReasonText() {
	case "authFailure":
		return "Fallas de autenticación"
	case "rcptToFailure":
		return "Destinatarios inválidos"
	case "loitering":
		return "Conexiones colgadas"
	case "portScanning":
		return "Escaneo de puertos"
	case "manual":
		return "Manual"
	case "", "other":
		return "Otro"
	}
	return e.ReasonText()
}

func (c *Client) ipList(ctx context.Context, object string) ([]IPEntry, error) {
	var list []IPEntry
	if err := c.get(ctx, object, nil, nil, &list); err != nil {
		return nil, err
	}
	// Lo más reciente primero.
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.After(list[j].CreatedAt) })
	return list, nil
}

func (c *Client) BlockedIPs(ctx context.Context) ([]IPEntry, error) {
	return c.ipList(ctx, "BlockedIp")
}
func (c *Client) AllowedIPs(ctx context.Context) ([]IPEntry, error) {
	return c.ipList(ctx, "AllowedIp")
}

// reload aplica a las conexiones los cambios de IPs ("ReloadBlockedIps" o "ReloadSettings").
func (c *Client) reload(ctx context.Context, action string) error {
	_, err := c.create(ctx, "Action", map[string]any{"@type": action})
	return err
}

// Unblock quita los bloqueos y los aplica de inmediato.
func (c *Client) Unblock(ctx context.Context, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := c.set(ctx, "BlockedIp", map[string]any{"destroy": ids}); err != nil {
		return err
	}
	return c.reload(ctx, "ReloadBlockedIps")
}

// Allow agrega una IP o red (CIDR) a las permitidas y la aplica de inmediato.
func (c *Client) Allow(ctx context.Context, address, reason string) error {
	address, err := NormalizeIPMask(address)
	if err != nil {
		return err
	}
	v := map[string]any{"address": address}
	if reason = strings.TrimSpace(reason); reason != "" {
		v["reason"] = reason
	}
	if _, err := c.create(ctx, "AllowedIp", v); err != nil {
		return err
	}
	return c.reload(ctx, "ReloadSettings")
}

// RemoveAllowed quita una IP permitida y la aplica de inmediato.
func (c *Client) RemoveAllowed(ctx context.Context, id string) error {
	if err := c.destroy(ctx, "AllowedIp", id); err != nil {
		return err
	}
	return c.reload(ctx, "ReloadSettings")
}

// NormalizeIPMask valida una IP o una red CIDR y la devuelve en su forma canónica.
func NormalizeIPMask(s string) (string, error) {
	s = strings.TrimSpace(s)
	if ip := net.ParseIP(s); ip != nil {
		return ip.String(), nil
	}
	if _, n, err := net.ParseCIDR(s); err == nil {
		return n.String(), nil
	}
	return "", fmt.Errorf("%q no es una IP ni una red válida (ej: 203.0.113.7 o 172.18.0.0/16)", s)
}

// Covers indica si la IP o red addr cubre la IP ip.
func Covers(addr, ip string) bool {
	target := net.ParseIP(ip)
	if target == nil {
		return false
	}
	if a := net.ParseIP(addr); a != nil {
		return a.Equal(target)
	}
	_, n, err := net.ParseCIDR(addr)
	return err == nil && n.Contains(target)
}

// Expires describe cuándo vence (los bloqueos automáticos suelen ser permanentes).
func (e IPEntry) Expires() string {
	if e.ExpiresAt == nil || e.ExpiresAt.IsZero() {
		return "nunca"
	}
	return e.ExpiresAt.Local().Format("2006-01-02 15:04")
}
