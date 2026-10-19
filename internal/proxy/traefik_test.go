package proxy

import (
	"strings"
	"testing"
)

func TestHSTSConfig(t *testing.T) {
	if !strings.Contains(hstsConfig, HSTSMiddleware+":") || !strings.Contains(hstsConfig, "stsSeconds: 31536000") {
		t.Fatal(hstsConfig)
	}
	// Sin includeSubDomains ni preload: no deben imponerse a subdominios fuera de world.
	if strings.Contains(strings.ToLower(hstsConfig), "subdomains") || strings.Contains(strings.ToLower(hstsConfig), "preload") {
		t.Fatal("no debe incluir includeSubDomains ni preload")
	}
}
