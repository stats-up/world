package mail

import (
	"context"
	"strings"
	"testing"
)

func TestNormalizeIPMaskAndCovers(t *testing.T) {
	ok := map[string]string{" 54.20.26.50 ": "54.20.26.50", "172.18.0.5/16": "172.18.0.0/16", "2001:db8::1": "2001:db8::1"}
	for in, want := range ok {
		if got, err := NormalizeIPMask(in); err != nil || got != want {
			t.Errorf("%q → %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "mx.statsup.cl", "1.2.3", "1.2.3.4/40"} {
		if _, err := NormalizeIPMask(bad); err == nil {
			t.Errorf("%q debió fallar", bad)
		}
	}
	if !Covers("172.18.0.0/16", "172.18.0.9") || Covers("172.18.0.0/16", "172.19.0.1") || !Covers("54.20.26.50", "54.20.26.50") || Covers("x", "1.1.1.1") {
		t.Error("Covers")
	}
	auth := "authFailure"
	if (IPEntry{Reason: &auth}).BlockReason() != "Fallas de autenticación" || (IPEntry{}).BlockReason() != "Otro" || (IPEntry{}).Expires() != "nunca" {
		t.Error("textos")
	}
}

// Los cambios de IPs solo se aplican tras recargar (comprobado contra Stalwart v0.16.25).
func TestUnblockAndAllowReload(t *testing.T) {
	var calls []string
	c := fakeStalwart(t, func(method string, args map[string]any) (string, any) {
		switch method {
		case "x:BlockedIp/set":
			calls = append(calls, "destroy:"+strings.Join(toStrings(args["destroy"]), ","))
			return method, map[string]any{"destroyed": args["destroy"]}
		case "x:AllowedIp/set":
			if cr, ok := args["create"].(map[string]any); ok {
				v := cr["n"].(map[string]any)
				calls = append(calls, "allow:"+v["address"].(string)+":"+str(v["reason"]))
				return method, map[string]any{"created": map[string]any{"n": map[string]any{"id": "a1"}}}
			}
			calls = append(calls, "remove:"+strings.Join(toStrings(args["destroy"]), ","))
			return method, map[string]any{"destroyed": args["destroy"]}
		case "x:Action/set":
			calls = append(calls, "reload:"+args["create"].(map[string]any)["n"].(map[string]any)["@type"].(string))
			return method, map[string]any{"created": map[string]any{"n": map[string]any{"id": "r"}}}
		}
		t.Fatalf("método %s", method)
		return "", nil
	})
	ctx := context.Background()
	if err := c.Unblock(ctx, "b1", "b2"); err != nil {
		t.Fatal(err)
	}
	if err := c.Allow(ctx, "172.18.0.7/16", " red docker "); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveAllowed(ctx, "a1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Allow(ctx, "no-es-ip", ""); err == nil {
		t.Error("IP inválida")
	}
	want := "destroy:b1,b2 reload:ReloadBlockedIps allow:172.18.0.0/16:red docker reload:ReloadSettings remove:a1 reload:ReloadSettings"
	if got := strings.Join(calls, " "); got != want {
		t.Errorf("llamadas:\n%s\nesperaba:\n%s", got, want)
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
