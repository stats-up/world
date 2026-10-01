package auth

import (
	"testing"
	"time"
)

// Vector de prueba del RFC 6238 (SHA1, secreto "12345678901234567890").
func TestTOTPRFCVector(t *testing.T) {
	secret := b32.EncodeToString([]byte("12345678901234567890"))
	cases := map[int64]string{59: "287082", 1111111109: "081804", 1234567890: "005924", 2000000000: "279037"}
	for ts, want := range cases {
		got, err := totpCode(secret, uint64(ts/30))
		if err != nil || got != want {
			t.Errorf("t=%d: got %s, want %s (err %v)", ts, got, want, err)
		}
		if !VerifyTOTP(secret, want, time.Unix(ts, 0)) {
			t.Errorf("t=%d: VerifyTOTP rechazó un código válido", ts)
		}
	}
	if VerifyTOTP(secret, "000000", time.Unix(59, 0)) {
		t.Error("VerifyTOTP aceptó un código inválido")
	}
}

func TestPassword(t *testing.T) {
	h, err := HashPassword("una-clave-larga")
	if err != nil {
		t.Fatal(err)
	}
	if !CheckPassword(h, "una-clave-larga") || CheckPassword(h, "otra") {
		t.Error("CheckPassword no se comporta como se espera")
	}
}
