package mail

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeStalwart responde /jmap/session y /jmap con un handler por método.
func fakeStalwart(t *testing.T, handle func(method string, args map[string]any) (string, any)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != AdminUser || p != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/jmap/session":
			w.Write([]byte(`{"accounts":{"d1":{}},"primaryAccounts":{}}`))
		case "/jmap":
			var req struct {
				MethodCalls [][]json.RawMessage `json:"methodCalls"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			var method string
			var args map[string]any
			json.Unmarshal(req.MethodCalls[0][0], &method)
			json.Unmarshal(req.MethodCalls[0][1], &args)
			if args["accountId"] != "d1" {
				t.Errorf("%s sin accountId", method)
			}
			name, res := handle(method, args)
			json.NewEncoder(w).Encode(map[string]any{"methodResponses": [][]any{{name, res, "0"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClient(nil, func() string { return "pw" })
	c.endpoint = func(context.Context) (string, error) { return srv.URL, nil }
	return c
}

func TestClientCreateAccount(t *testing.T) {
	var got map[string]any
	c := fakeStalwart(t, func(method string, args map[string]any) (string, any) {
		if method != "x:Account/set" {
			t.Fatalf("método %s", method)
		}
		got = args["create"].(map[string]any)["n"].(map[string]any)
		return method, map[string]any{"created": map[string]any{"n": map[string]any{"id": "acc1"}}}
	})
	id, err := c.CreateAccount(context.Background(), NewAccount{DomainID: "c", Name: "ventas", Password: "secreto-largo",
		Quota: 5 << 30, Aliases: []string{"contacto", "info"}})
	if err != nil || id != "acc1" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	if got["@type"] != "User" || got["domainId"] != "c" || got["name"] != "ventas" {
		t.Errorf("cuenta: %v", got)
	}
	if q := got["quotas"].(map[string]any)["maxDiskQuota"]; q != float64(5<<30) {
		t.Errorf("cuota: %v", q)
	}
	aliases := got["aliases"].(map[string]any)
	if len(aliases) != 2 || aliases["1"].(map[string]any)["name"] != "info" {
		t.Errorf("alias: %v", aliases)
	}
	cred := got["credentials"].(map[string]any)["0"].(map[string]any)
	if cred["@type"] != "Password" || cred["secret"] != "secreto-largo" {
		t.Errorf("credencial: %v", cred)
	}
}

func TestClientErrors(t *testing.T) {
	c := fakeStalwart(t, func(method string, args map[string]any) (string, any) {
		if method == "x:Account/set" {
			return method, map[string]any{"notUpdated": map[string]any{"a": map[string]any{
				"type": "invalidProperties", "description": "Password must be at least 8 characters long.", "properties": []string{"secret"}}}}
		}
		return "error", map[string]any{"type": "forbidden"}
	})
	err := c.SetPassword(context.Background(), "a", "corta")
	var me *MethodError
	if !errors.As(err, &me) || !strings.Contains(err.Error(), "at least 8") {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.Domains(context.Background()); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("err = %v", err)
	}

	bad := NewClient(nil, func() string { return "otra" })
	bad.endpoint = c.endpoint
	if _, err := bad.Domains(context.Background()); err == nil || !strings.Contains(err.Error(), "credencial") {
		t.Fatalf("err = %v", err)
	}
}

func TestClientDomains(t *testing.T) {
	c := fakeStalwart(t, func(method string, args map[string]any) (string, any) {
		switch method {
		case "x:Domain/get":
			return method, map[string]any{"list": []map[string]any{
				{"id": "c", "name": "prueba.frikiforja.cl", "dnsManagement": map[string]any{"@type": "Automatic", "origin": "frikiforja.cl", "publishRecords": map[string]bool{"mx": true, "dkim": true}}},
				{"id": "b", "name": "mx.statsup.cl", "dnsManagement": map[string]any{"@type": "Automatic", "publishRecords": map[string]bool{"caa": true}}},
			}}
		case "x:Account/query":
			if args["filter"].(map[string]any)["domainId"] == "c" {
				return method, map[string]any{"ids": []string{"a1", "a2"}}
			}
			return method, map[string]any{"ids": []string{}}
		}
		t.Fatalf("método %s", method)
		return "", nil
	})
	ds, err := c.Domains(context.Background())
	if err != nil || len(ds) != 2 {
		t.Fatalf("%v %v", ds, err)
	}
	if ds[1].Name != "prueba.frikiforja.cl" || !ds[1].Active || ds[1].Accounts != 2 || ds[1].Origin != "frikiforja.cl" {
		t.Errorf("dominio: %+v", ds[1])
	}
	if ds[0].Active {
		t.Errorf("mx.statsup.cl no publica MX: %+v", ds[0])
	}
}

const zone = `prueba.frikiforja.cl. IN TXT "v=spf1 mx -all"
prueba.frikiforja.cl. IN MX 10 mx.statsup.cl.
_dmarc.prueba.frikiforja.cl. IN TXT "v=DMARC1; p=reject; rua=mailto:postmaster@prueba.frikiforja.cl"
_imaps._tcp.prueba.frikiforja.cl. IN SRV 0 1 993 mx.statsup.cl.
mta-sts.prueba.frikiforja.cl. IN CNAME mx.statsup.cl.
_mta-sts.prueba.frikiforja.cl. IN TXT "v=STSv1; id=1"
v1-ed25519-20261006._domainkey.prueba.frikiforja.cl. IN TXT "v=DKIM1; k=ed25519; h=sha256; p=TMN="
autoconfig.prueba.frikiforja.cl. IN CNAME mx.statsup.cl.
`

func TestParseZone(t *testing.T) {
	recs := ParseZone(zone)
	var got []string
	for _, r := range recs {
		got = append(got, r.Type+" "+r.Name+" = "+r.Value)
	}
	want := []string{
		"TXT prueba.frikiforja.cl = v=spf1 mx -all",
		"MX prueba.frikiforja.cl = 10 mx.statsup.cl",
		"TXT _dmarc.prueba.frikiforja.cl = v=DMARC1; p=reject; rua=mailto:postmaster@prueba.frikiforja.cl",
		"TXT v1-ed25519-20261006._domainkey.prueba.frikiforja.cl = v=DKIM1; k=ed25519; h=sha256; p=TMN=",
		"CNAME autoconfig.prueba.frikiforja.cl = mx.statsup.cl",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s", strings.Join(got, "\n"))
	}
}

func TestStatusFor(t *testing.T) {
	spf := Record{Type: "TXT", Value: "v=spf1 mx -all"}
	same := func(v string) bool { return normTXT(v) == normTXT(spf.Value) }
	similar := func(v string) bool { return strings.HasPrefix(v, "v=spf1") }
	notFound := &net.DNSError{IsNotFound: true}
	cases := []struct {
		found []string
		err   error
		want  string
	}{
		{[]string{"google-site-verification=x", "v=spf1 mx -all"}, nil, "ok"},
		{[]string{"v=spf1 ip4:38.18.230.11 ~all"}, nil, "distinto"},
		{[]string{"google-site-verification=x"}, nil, "falta"},
		{nil, notFound, "falta"},
		{nil, errors.New("timeout"), "error"},
	}
	for _, c := range cases {
		if got := statusFor(c.found, c.err, same, similar); got != c.want {
			t.Errorf("%v %v: %s, want %s", c.found, c.err, got, c.want)
		}
	}
}

func TestDefaultOriginAndQuota(t *testing.T) {
	for in, want := range map[string]string{"frikiforja.cl": "", "prueba.frikiforja.cl": "frikiforja.cl", "a.b.statsup.cl": "statsup.cl"} {
		if got := DefaultOrigin(in); got != want {
			t.Errorf("DefaultOrigin(%s) = %q", in, got)
		}
	}
	if p := (Account{Quota: 100, Used: 25}).UsedPercent(); p != 25 {
		t.Errorf("%d", p)
	}
	if p := (Account{Quota: 0, Used: 25}).UsedPercent(); p != 0 {
		t.Errorf("%d", p)
	}
	if p := (Account{Quota: 10, Used: 25}).UsedPercent(); p != 100 {
		t.Errorf("%d", p)
	}
}
