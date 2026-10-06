package mail

import (
	"context"
	"net"
	"strings"
	"sync"
	"time"
)

// Record es un registro DNS que el dominio debería tener, con su estado en el DNS público.
type Record struct {
	Name   string // nombre completo, sin punto final
	Type   string // MX, TXT, CNAME
	Value  string // valor esperado (MX: "10 mx.statsup.cl")
	Status string // ok | distinto | falta | error
	Found  string // lo que hay publicado, si es distinto
	Note   string
}

// ParseZone lee el zone file que genera Stalwart y devuelve los registros que vale la pena verificar.
func ParseZone(zone string) []Record {
	var out []Record
	for _, line := range strings.Split(zone, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[1] != "IN" {
			continue
		}
		name, typ := strings.TrimSuffix(f[0], "."), f[2]
		value := strings.Join(f[3:], " ")
		switch typ {
		case "MX":
			value = strings.TrimSuffix(value, ".")
		case "CNAME":
			value = strings.TrimSuffix(value, ".")
			// Solo los de configuración automática de clientes; mta-sts y ua-auto-config son opcionales.
			if !strings.HasPrefix(name, "autoconfig.") && !strings.HasPrefix(name, "autodiscover.") {
				continue
			}
		case "TXT":
			value = strings.Trim(value, `"`)
			if !isMailTXT(name, value) {
				continue
			}
		default:
			continue
		}
		out = append(out, Record{Name: name, Type: typ, Value: value, Note: recordNote(name, typ, value)})
	}
	return out
}

func isMailTXT(name, value string) bool {
	return strings.HasPrefix(value, "v=spf1") || strings.HasPrefix(value, "v=DMARC1") ||
		strings.Contains(name, "._domainkey.")
}

func recordNote(name, typ, value string) string {
	switch {
	case typ == "MX":
		return "Recepción: el correo del dominio llega a este servidor"
	case strings.HasPrefix(value, "v=spf1"):
		return "SPF: si ya hay un SPF, combínalos en uno solo"
	case strings.HasPrefix(value, "v=DMARC1"):
		return "DMARC: si ya hay uno, no lo bajes sin motivo"
	case strings.Contains(name, "._domainkey."):
		return "DKIM: firma de los correos enviados"
	case typ == "CNAME":
		return "Configuración automática de Thunderbird/Outlook (nube gris)"
	}
	return ""
}

// Resolver consulta un DNS público (Cloudflare) para no depender de la caché del servidor.
var Resolver = &net.Resolver{
	PreferGo: true,
	Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		d := net.Dialer{Timeout: 5 * time.Second}
		return d.DialContext(ctx, network, "1.1.1.1:53")
	},
}

// CheckRecords revisa en paralelo cada registro en el DNS público y completa su Status.
func CheckRecords(ctx context.Context, r *net.Resolver, recs []Record) []Record {
	out := make([]Record, len(recs))
	copy(out, recs)
	var wg sync.WaitGroup
	for i := range out {
		wg.Add(1)
		go func(rec *Record) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			checkRecord(ctx, r, rec)
		}(&out[i])
	}
	wg.Wait()
	return out
}

func checkRecord(ctx context.Context, r *net.Resolver, rec *Record) {
	var found []string
	var err error
	switch rec.Type {
	case "MX":
		var mxs []*net.MX
		mxs, err = r.LookupMX(ctx, rec.Name)
		for _, mx := range mxs {
			found = append(found, strings.TrimSuffix(mx.Host, "."))
		}
		// Se compara solo el servidor: la prioridad no cambia el resultado si hay un solo MX.
		_, host, _ := strings.Cut(rec.Value, " ")
		rec.Status = statusFor(found, err, func(v string) bool { return strings.EqualFold(v, host) }, nil)
	case "CNAME":
		var target string
		target, err = r.LookupCNAME(ctx, rec.Name)
		if err == nil {
			found = []string{strings.TrimSuffix(target, ".")}
		}
		rec.Status = statusFor(found, err, func(v string) bool { return strings.EqualFold(v, rec.Value) }, nil)
	case "TXT":
		found, err = r.LookupTXT(ctx, rec.Name)
		prefix := ""
		if i := strings.IndexByte(rec.Value, ';'); i > 0 && !strings.HasPrefix(rec.Value, "v=spf1") {
			prefix = rec.Value[:i] // v=DMARC1 / v=DKIM1
		} else if strings.HasPrefix(rec.Value, "v=spf1") {
			prefix = "v=spf1"
		}
		same := func(v string) bool { return normTXT(v) == normTXT(rec.Value) }
		similar := func(v string) bool { return prefix != "" && strings.HasPrefix(v, prefix) }
		rec.Status = statusFor(found, err, same, similar)
	}
	if rec.Status == "distinto" {
		rec.Found = strings.Join(found, " | ")
	}
}

func normTXT(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "; ", ";")), " ")
}

func statusFor(found []string, err error, same, similar func(string) bool) string {
	if err != nil {
		if dnsErr, ok := err.(*net.DNSError); ok && dnsErr.IsNotFound {
			return "falta"
		}
		return "error"
	}
	for _, v := range found {
		if same(v) {
			return "ok"
		}
	}
	for _, v := range found {
		if similar != nil && similar(v) {
			return "distinto"
		}
	}
	if len(found) > 0 && similar == nil {
		return "distinto"
	}
	return "falta"
}
