package bucket

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Las subidas van firmadas sin calcular el hash del cuerpo (UNSIGNED-PAYLOAD, solo por HTTPS):
// así un archivo grande se envía en streaming sin leerlo dos veces.
const unsignedPayload = "UNSIGNED-PAYLOAD"

// keyRe limita las llaves a caracteres que no necesitan codificarse en la URL ni en la firma.
var keyRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// objectURL arma la URL de un objeto (mismo estilo que listURL).
func (c Creds) objectURL(key string) (string, error) {
	if !keyRe.MatchString(key) || strings.Contains(key, "..") || strings.Contains(key, "//") {
		return "", fmt.Errorf("nombre de archivo inválido en el bucket: %q", key)
	}
	u, err := c.listURL(nil)
	if err != nil {
		return "", err
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + key
	u.RawQuery = ""
	return u.String(), nil
}

func (c Creds) do(ctx context.Context, hc *http.Client, method, key string, body io.Reader, size int64) (*http.Response, error) {
	u, err := c.objectURL(key)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = size
		signPayload(req, c, time.Now(), unsignedPayload)
	} else {
		sign(req, c, time.Now())
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("no se pudo conectar con el bucket: %w", err)
	}
	return resp, nil
}

func checkStatus(resp *http.Response, ok ...int) error {
	for _, code := range ok {
		if resp.StatusCode == code {
			return nil
		}
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return s3Error(resp.StatusCode, body)
}

// Put sube size bytes de body a key (un solo PUT: hasta 5 GB).
func Put(ctx context.Context, hc *http.Client, c Creds, key string, body io.Reader, size int64) error {
	resp, err := c.do(ctx, hc, http.MethodPut, key, body, size)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return checkStatus(resp, http.StatusOK)
}

// ErrNotFound: el objeto no existe en el bucket.
var ErrNotFound = errors.New("el archivo ya no está en el bucket")

// Get abre un objeto para leerlo. Quien llama cierra el cuerpo.
func Get(ctx context.Context, hc *http.Client, c Creds, key string) (io.ReadCloser, int64, error) {
	resp, err := c.do(ctx, hc, http.MethodGet, key, nil, 0)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, 0, ErrNotFound
	}
	if err := checkStatus(resp, http.StatusOK); err != nil {
		resp.Body.Close()
		return nil, 0, err
	}
	return resp.Body, resp.ContentLength, nil
}

// Delete borra un objeto (borrar uno que no existe no es error).
func Delete(ctx context.Context, hc *http.Client, c Creds, key string) error {
	resp, err := c.do(ctx, hc, http.MethodDelete, key, nil, 0)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return checkStatus(resp, http.StatusNoContent, http.StatusOK, http.StatusNotFound)
}
