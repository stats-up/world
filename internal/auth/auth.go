// Package auth: contraseñas (bcrypt), tokens aleatorios y TOTP (RFC 6238) para 2FA.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const MinPasswordLen = 10

func HashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(h), err
}

func CheckPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

var (
	dummyOnce sync.Once
	dummyHash []byte
)

// DummyCheckPassword tarda lo mismo que CheckPassword pero siempre falla. Se usa cuando el
// usuario no existe, para que el tiempo de respuesta no revele qué correos tienen cuenta.
func DummyCheckPassword(pw string) {
	dummyOnce.Do(func() {
		dummyHash, _ = bcrypt.GenerateFromPassword([]byte(RandomToken(16)), bcrypt.DefaultCost)
	})
	bcrypt.CompareHashAndPassword(dummyHash, []byte(pw))
}

// RandomToken devuelve n bytes aleatorios codificados en base64url.
func RandomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// HashToken: los tokens de sesión se guardan hasheados, nunca en claro.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func SecureEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func NewTOTPSecret() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b32.EncodeToString(b)
}

func totpCode(secret string, counter uint64) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return "", err
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", v%1_000_000), nil
}

// VerifyTOTP acepta el código actual y el de ±30 segundos (desfase de reloj).
func VerifyTOTP(secret, code string, now time.Time) bool {
	_, ok := MatchTOTP(secret, code, now)
	return ok
}

// MatchTOTP verifica el código (se acepta el intervalo anterior y el siguiente por desfase de reloj)
// y devuelve el intervalo de 30 s al que corresponde, para no aceptar el mismo código dos veces.
func MatchTOTP(secret, code string, now time.Time) (int64, bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != 6 || secret == "" {
		return 0, false
	}
	step := uint64(now.Unix() / 30)
	for _, c := range []uint64{step - 1, step, step + 1} {
		want, err := totpCode(secret, c)
		if err == nil && SecureEqual(want, code) {
			return int64(c), true
		}
	}
	return 0, false
}

// TOTPURL es el contenido del QR que leen Google Authenticator, Authy, 1Password, etc.
func TOTPURL(issuer, account, secret string) string {
	v := url.Values{}
	v.Set("secret", secret)
	v.Set("issuer", issuer)
	return "otpauth://totp/" + url.PathEscape(issuer+":"+account) + "?" + v.Encode()
}

// TOTPCodeAt devuelve el código válido en el instante t (para pruebas).
func TOTPCodeAt(secret string, t time.Time) (string, error) {
	return totpCode(secret, uint64(t.Unix()/30))
}
