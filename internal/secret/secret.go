// Package secret cifra datos sensibles (variables de ambiente, deploy keys)
// con AES-256-GCM usando una llave local que nunca sale del servidor.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
)

type Box struct {
	aead cipher.AEAD
}

// LoadOrCreate lee la llave desde path, o la genera (32 bytes, permisos 0600) si no existe.
func LoadOrCreate(path string) (*Box, error) {
	key, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, key, 0o600); err != nil {
			return nil, fmt.Errorf("guardando llave: %w", err)
		}
	} else if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("llave inválida en %s: se esperaban 32 bytes", path)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

func (b *Box) Seal(plain []byte) []byte {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	return b.aead.Seal(nonce, nonce, plain, nil)
}

func (b *Box) Open(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, nil
	}
	n := b.aead.NonceSize()
	if len(data) < n {
		return nil, errors.New("dato cifrado inválido")
	}
	return b.aead.Open(nil, data[:n], data[n:], nil)
}

func (b *Box) SealString(s string) []byte {
	if s == "" {
		return nil
	}
	return b.Seal([]byte(s))
}

func (b *Box) OpenString(data []byte) (string, error) {
	p, err := b.Open(data)
	return string(p), err
}
