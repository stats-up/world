package mail

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
)

// Registros que Stalwart publica solo en Cloudflare según el modo del dominio.
// SPF, DMARC y CAA nunca se publican solos en dominios reales: suelen tener valores
// de otros servicios (cPanel, MailBaby, AutoSSL) que no se deben pisar. world los
// muestra en la tabla de DNS para crearlos a mano.
var (
	// Migración: el correo todavía llega a otro servidor (cPanel). Solo DKIM, para
	// tener las llaves publicadas antes del corte.
	publishMigration = []string{"dkim"}
	// Activo: este servidor recibe el correo del dominio (incluye el MX).
	publishActive = []string{"mx", "dkim", "srv", "autoConfig", "autoDiscover"}
)

type Domain struct {
	ID          string
	Name        string
	Description string
	Origin      string // zona DNS en Cloudflare (vacío = el mismo dominio)
	Active      bool   // publica el MX: este servidor recibe el correo
	ZoneFile    string // registros que Stalwart recomienda para el dominio
	Accounts    int
}

type domainJSON struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	DNSZoneFile   string `json:"dnsZoneFile"`
	DNSManagement struct {
		Type           string          `json:"@type"`
		DNSServerID    string          `json:"dnsServerId"`
		Origin         string          `json:"origin"`
		PublishRecords map[string]bool `json:"publishRecords"`
	} `json:"dnsManagement"`
}

func (d domainJSON) toDomain() Domain {
	return Domain{ID: d.ID, Name: d.Name, Description: d.Description, Origin: d.DNSManagement.Origin,
		Active: d.DNSManagement.PublishRecords["mx"], ZoneFile: d.DNSZoneFile}
}

// IsServerDomain: el dominio que existe solo para el certificado del nombre del servidor (mx.statsup.cl).
// No se listan ni se pueden borrar desde world.
func IsServerDomain(name, hostname string) bool { return name == hostname }

func (c *Client) Domains(ctx context.Context) ([]Domain, error) {
	var list []domainJSON
	if err := c.get(ctx, "Domain", nil, []string{"name", "description", "dnsManagement"}, &list); err != nil {
		return nil, err
	}
	out := make([]Domain, 0, len(list))
	for _, d := range list {
		dom := d.toDomain()
		ids, err := c.query(ctx, "Account", map[string]any{"domainId": d.ID})
		if err != nil {
			return nil, err
		}
		dom.Accounts = len(ids)
		out = append(out, dom)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (c *Client) Domain(ctx context.Context, id string) (*Domain, error) {
	var list []domainJSON
	if err := c.get(ctx, "Domain", []string{id}, []string{"name", "description", "dnsManagement", "dnsZoneFile"}, &list); err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNotFound
	}
	d := list[0].toDomain()
	return &d, nil
}

var ErrNotFound = errors.New("no existe en Stalwart")

// firstID devuelve el primer objeto configurado de un tipo (DnsServer, AcmeProvider).
func (c *Client) firstID(ctx context.Context, object, what string) (string, error) {
	ids, err := c.query(ctx, object, nil)
	if err != nil {
		return "", err
	}
	if len(ids) == 0 {
		return "", errors.New("Stalwart no tiene " + what + " configurado")
	}
	return ids[0], nil
}

func publishSet(active bool) map[string]bool {
	list := publishMigration
	if active {
		list = publishActive
	}
	set := map[string]bool{}
	for _, r := range list {
		set[r] = true
	}
	return set
}

func (c *Client) dnsManagement(ctx context.Context, origin string, active bool) (map[string]any, error) {
	dnsID, err := c.firstID(ctx, "DnsServer", "un servidor DNS (Cloudflare)")
	if err != nil {
		return nil, err
	}
	var o any
	if origin != "" {
		o = origin
	}
	return map[string]any{"@type": "Automatic", "dnsServerId": dnsID, "origin": o, "publishRecords": publishSet(active)}, nil
}

// CreateDomain crea el dominio con certificado automático (DNS-01) y DKIM automático.
func (c *Client) CreateDomain(ctx context.Context, name, origin, description string, active bool) (string, error) {
	acmeID, err := c.firstID(ctx, "AcmeProvider", "un proveedor de certificados (ACME)")
	if err != nil {
		return "", err
	}
	dns, err := c.dnsManagement(ctx, origin, active)
	if err != nil {
		return "", err
	}
	return c.create(ctx, "Domain", map[string]any{
		"name":        name,
		"aliases":     map[string]bool{},
		"description": nullable(description),
		// Sin SANs: certificado comodín (*.dominio), que cubre mail.<dominio> para el webmail.
		"certificateManagement": map[string]any{"@type": "Automatic", "acmeProviderId": acmeID, "subjectAlternativeNames": map[string]bool{}},
		"dkimManagement":        map[string]any{"@type": "Automatic"},
		"dnsManagement":         dns,
		"subAddressing":         map[string]any{"@type": "Enabled"},
	})
}

// SetDomainActive cambia entre "migración" y "activo" (publicar o no el MX en Cloudflare).
func (c *Client) SetDomainActive(ctx context.Context, id string, active bool) error {
	d, err := c.Domain(ctx, id)
	if err != nil {
		return err
	}
	dns, err := c.dnsManagement(ctx, d.Origin, active)
	if err != nil {
		return err
	}
	return c.update(ctx, "Domain", id, map[string]any{"dnsManagement": dns})
}

func (c *Client) DeleteDomain(ctx context.Context, id string) error {
	ids, err := c.query(ctx, "Account", map[string]any{"domainId": id})
	if err != nil {
		return err
	}
	if len(ids) > 0 {
		return errors.New("el dominio todavía tiene buzones: elimínalos primero")
	}
	return c.destroy(ctx, "Domain", id)
}

// DefaultOrigin adivina la zona de Cloudflare: los dos últimos niveles (frikiforja.cl para prueba.frikiforja.cl).
func DefaultOrigin(name string) string {
	parts := strings.Split(name, ".")
	if len(parts) <= 2 {
		return ""
	}
	return strings.Join(parts[len(parts)-2:], ".")
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// --- Buzones ---

type Account struct {
	ID          string
	Name        string // parte local (antes de @)
	Email       string
	Description string
	Quota       int64 // bytes; 0 = sin límite
	Used        int64
	Aliases     []string // partes locales, en el mismo dominio
}

// UsedPercent para la barra de cuota (0 si no hay límite).
func (a Account) UsedPercent() int {
	if a.Quota <= 0 {
		return 0
	}
	p := int(a.Used * 100 / a.Quota)
	if p > 100 {
		p = 100
	}
	return p
}

type accountJSON struct {
	ID           string           `json:"id"`
	Name         string           `json:"name"`
	Email        string           `json:"emailAddress"`
	Description  string           `json:"description"`
	Quotas       map[string]int64 `json:"quotas"`
	UsedDisk     int64            `json:"usedDiskQuota"`
	AliasesByIdx map[string]struct {
		Name string `json:"name"`
	} `json:"aliases"`
}

var accountProps = []string{"name", "emailAddress", "description", "quotas", "usedDiskQuota", "aliases"}

func (a accountJSON) toAccount() Account {
	acc := Account{ID: a.ID, Name: a.Name, Email: a.Email, Description: a.Description, Quota: a.Quotas["maxDiskQuota"], Used: a.UsedDisk}
	for _, al := range a.AliasesByIdx {
		acc.Aliases = append(acc.Aliases, al.Name)
	}
	sort.Strings(acc.Aliases)
	return acc
}

func (c *Client) Accounts(ctx context.Context, domainID string) ([]Account, error) {
	ids, err := c.query(ctx, "Account", map[string]any{"domainId": domainID})
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	var list []accountJSON
	if err := c.get(ctx, "Account", ids, accountProps, &list); err != nil {
		return nil, err
	}
	out := make([]Account, 0, len(list))
	for _, a := range list {
		out = append(out, a.toAccount())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (c *Client) Account(ctx context.Context, id string) (*Account, error) {
	var list []accountJSON
	if err := c.get(ctx, "Account", []string{id}, accountProps, &list); err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNotFound
	}
	a := list[0].toAccount()
	return &a, nil
}

// Las listas de Stalwart se codifican como objetos {"0": ..., "1": ...}.
func aliasList(domainID string, aliases []string) map[string]any {
	out := map[string]any{}
	for i, a := range aliases {
		out[strconv.Itoa(i)] = map[string]any{"name": a, "domainId": domainID, "enabled": true}
	}
	return out
}

func quotaMap(quota int64) map[string]int64 {
	if quota <= 0 {
		return map[string]int64{}
	}
	return map[string]int64{"maxDiskQuota": quota}
}

type NewAccount struct {
	DomainID    string
	Name        string
	Description string
	Password    string
	Quota       int64
	Aliases     []string
}

func (c *Client) CreateAccount(ctx context.Context, a NewAccount) (string, error) {
	return c.create(ctx, "Account", map[string]any{
		"@type":            "User",
		"name":             a.Name,
		"domainId":         a.DomainID,
		"description":      nullable(a.Description),
		"credentials":      map[string]any{"0": map[string]any{"@type": "Password", "secret": a.Password}},
		"quotas":           quotaMap(a.Quota),
		"aliases":          aliasList(a.DomainID, a.Aliases),
		"roles":            map[string]any{"@type": "User"},
		"permissions":      map[string]any{"@type": "Inherit"},
		"encryptionAtRest": map[string]any{"@type": "Disabled"},
		"memberGroupIds":   map[string]bool{},
	})
}

// SetPassword reemplaza las contraseñas de la cuenta por una nueva.
func (c *Client) SetPassword(ctx context.Context, id, password string) error {
	return c.update(ctx, "Account", id, map[string]any{
		"credentials": map[string]any{"0": map[string]any{"@type": "Password", "secret": password}},
	})
}

func (c *Client) UpdateAccount(ctx context.Context, id, domainID, description string, quota int64, aliases []string) error {
	return c.update(ctx, "Account", id, map[string]any{
		"description": nullable(description),
		"quotas":      quotaMap(quota),
		"aliases":     aliasList(domainID, aliases),
	})
}

func (c *Client) DeleteAccount(ctx context.Context, id string) error {
	return c.destroy(ctx, "Account", id)
}
