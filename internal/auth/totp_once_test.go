package auth

import (
	"testing"
	"time"
)

func TestMatchTOTPStep(t *testing.T) {
	secret := NewTOTPSecret()
	now := time.Unix(1_800_000_000, 0)
	code, err := TOTPCodeAt(secret, now)
	if err != nil {
		t.Fatal(err)
	}
	step, ok := MatchTOTP(secret, code, now)
	if !ok || step != now.Unix()/30 {
		t.Fatalf("step %d ok %v", step, ok)
	}
	// El código del intervalo anterior sigue valiendo (desfase de reloj) y se informa ese intervalo.
	if step, ok := MatchTOTP(secret, code, now.Add(30*time.Second)); !ok || step != now.Unix()/30 {
		t.Errorf("intervalo siguiente: %d %v", step, ok)
	}
	if _, ok := MatchTOTP(secret, code, now.Add(2*time.Minute)); ok {
		t.Error("código vencido aceptado")
	}
	DummyCheckPassword("cualquier-cosa") // no debe entrar en pánico
}
