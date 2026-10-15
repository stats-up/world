// Package bucket mide el espacio usado en buckets S3 (Lightsail, AWS u otros compatibles)
// y por carpeta. Solo lee: lista los objetos con ListObjectsV2, nunca escribe ni borra.
package bucket

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Creds identifica un bucket y las llaves para leerlo.
type Creds struct {
	Bucket    string
	Region    string // ej: sa-east-1
	Endpoint  string // vacío = AWS/Lightsail; si no, URL base de un S3 compatible (R2, MinIO…)
	AccessKey string
	SecretKey string
}

type Object struct {
	Key  string
	Size int64
}

// sha256 de un cuerpo vacío (todas las peticiones son GET sin cuerpo).
const emptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// listURL arma la URL del bucket: virtual-hosted en AWS, path-style en endpoints propios.
func (c Creds) listURL(q url.Values) (*url.URL, error) {
	var u *url.URL
	if c.Endpoint == "" {
		u = &url.URL{Scheme: "https", Host: c.Bucket + ".s3." + c.Region + ".amazonaws.com", Path: "/"}
	} else {
		base, err := url.Parse(strings.TrimRight(c.Endpoint, "/"))
		if err != nil || base.Host == "" {
			return nil, errors.New("endpoint inválido")
		}
		u = &url.URL{Scheme: base.Scheme, Host: base.Host, Path: base.Path + "/" + c.Bucket}
	}
	u.RawQuery = canonicalQuery(q)
	return u, nil
}

// List devuelve hasta 1000 objetos desde el token de continuación ("" = el inicio).
func List(ctx context.Context, hc *http.Client, c Creds, token string, max int) ([]Object, string, error) {
	q := url.Values{"list-type": {"2"}, "max-keys": {strconv.Itoa(max)}}
	if token != "" {
		q.Set("continuation-token", token)
	}
	u, err := c.listURL(q)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", err
	}
	sign(req, c, time.Now())
	resp, err := hc.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("no se pudo conectar con el bucket: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", s3Error(resp.StatusCode, body)
	}
	var out struct {
		Contents []struct {
			Key  string
			Size int64
		}
		IsTruncated           bool
		NextContinuationToken string
	}
	if err := xml.Unmarshal(body, &out); err != nil {
		return nil, "", fmt.Errorf("respuesta inesperada del bucket: %w", err)
	}
	objs := make([]Object, len(out.Contents))
	for i, o := range out.Contents {
		objs[i] = Object{Key: o.Key, Size: o.Size}
	}
	next := ""
	if out.IsTruncated {
		next = out.NextContinuationToken
		if next == "" {
			return nil, "", errors.New("el bucket indicó más resultados pero sin token de continuación")
		}
	}
	return objs, next, nil
}

// s3Error traduce los errores comunes a algo accionable.
func s3Error(status int, body []byte) error {
	var e struct{ Code, Message string }
	xml.Unmarshal(body, &e)
	switch e.Code {
	case "InvalidAccessKeyId", "SignatureDoesNotMatch":
		return fmt.Errorf("las llaves de acceso no son válidas (%s)", e.Code)
	case "AccessDenied":
		return errors.New("las llaves no tienen permiso para listar este bucket (AccessDenied)")
	case "NoSuchBucket":
		return errors.New("el bucket no existe (NoSuchBucket)")
	case "PermanentRedirect", "AuthorizationHeaderMalformed", "IllegalLocationConstraintException":
		return fmt.Errorf("la región no corresponde a la del bucket (%s)", e.Code)
	case "":
		return fmt.Errorf("el bucket respondió HTTP %d", status)
	}
	return fmt.Errorf("%s: %s", e.Code, e.Message)
}

// sign firma la petición con AWS Signature V4 (cabeceras host, x-amz-content-sha256 y x-amz-date).
func sign(req *http.Request, c Creds, now time.Time) {
	amzDate := now.UTC().Format("20060102T150405Z")
	day := amzDate[:8]
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", emptyHash)
	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	const signed = "host;x-amz-content-sha256;x-amz-date"
	creq := strings.Join([]string{
		req.Method, path, canonicalQuery(req.URL.Query()),
		"host:" + req.URL.Host + "\nx-amz-content-sha256:" + emptyHash + "\nx-amz-date:" + amzDate + "\n",
		signed, emptyHash,
	}, "\n")
	scope := day + "/" + c.Region + "/s3/aws4_request"
	h := sha256.Sum256([]byte(creq))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(h[:])
	key := []byte("AWS4" + c.SecretKey)
	for _, part := range []string{day, c.Region, "s3", "aws4_request"} {
		key = hmacSHA256(key, part)
	}
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.AccessKey+"/"+scope+
		", SignedHeaders="+signed+", Signature="+hex.EncodeToString(hmacSHA256(key, toSign)))
}

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}

// canonicalQuery: parámetros ordenados y codificados como pide SigV4 (espacio = %20).
func canonicalQuery(v url.Values) string {
	esc := func(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := append([]string(nil), v[k]...)
		sort.Strings(vals)
		for _, val := range vals {
			parts = append(parts, esc(k)+"="+esc(val))
		}
	}
	return strings.Join(parts, "&")
}
