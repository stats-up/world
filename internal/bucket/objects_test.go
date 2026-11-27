package bucket

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestObjects(t *testing.T) {
	var mu sync.Mutex
	objs := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AK/") {
			http.Error(w, "sin firma", http.StatusForbidden)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/mi-bucket/")
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			if r.Header.Get("x-amz-content-sha256") != unsignedPayload || r.ContentLength < 0 {
				http.Error(w, "falta el largo", http.StatusBadRequest)
				return
			}
			b, _ := io.ReadAll(r.Body)
			objs[key] = string(b)
		case http.MethodGet:
			v, ok := objs[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte("<Error><Code>NoSuchKey</Code></Error>"))
				return
			}
			w.Write([]byte(v))
		case http.MethodDelete:
			delete(objs, key)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	c := Creds{Bucket: "mi-bucket", Region: "us-east-1", Endpoint: srv.URL, AccessKey: "AK", SecretKey: "SK"}
	ctx, hc := context.Background(), srv.Client()

	if err := Put(ctx, hc, c, "world-backups/tienda/20261127-030000.sql.gz", strings.NewReader("datos"), 5); err != nil {
		t.Fatal(err)
	}
	body, size, err := Get(ctx, hc, c, "world-backups/tienda/20261127-030000.sql.gz")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(body)
	body.Close()
	if string(b) != "datos" || size != 5 {
		t.Errorf("get: %q %d", b, size)
	}
	if err := Delete(ctx, hc, c, "world-backups/tienda/20261127-030000.sql.gz"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Get(ctx, hc, c, "world-backups/tienda/20261127-030000.sql.gz"); !errors.Is(err, ErrNotFound) {
		t.Errorf("después de borrar: %v", err)
	}
	for _, bad := range []string{"../x", "a//b", "con espacio", "/abs"} {
		if err := Put(ctx, hc, c, bad, strings.NewReader(""), 0); err == nil {
			t.Errorf("llave %q debió rechazarse", bad)
		}
	}
}
