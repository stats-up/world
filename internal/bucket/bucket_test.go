package bucket

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"world/internal/secret"
	"world/internal/store"
)

// Ejemplo "GET Bucket (List Objects)" de la documentación de AWS Signature V4 para S3.
func TestSignAWSExample(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/?max-keys=2&prefix=J", nil)
	c := Creds{Region: "us-east-1", AccessKey: "AKIAIOSFODNN7EXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}
	sign(req, c, time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("firma:\n got %s\nwant %s", got, want)
	}
}

func TestCanonicalQuery(t *testing.T) {
	q := map[string][]string{"prefix": {"a b~*"}, "continuation-token": {"1/x+y="}, "list-type": {"2"}}
	if got := canonicalQuery(q); got != "continuation-token=1%2Fx%2By%3D&list-type=2&prefix=a%20b~%2A" {
		t.Fatal(got)
	}
}

func TestListURL(t *testing.T) {
	u, _ := Creds{Bucket: "b1", Region: "sa-east-1"}.listURL(nil)
	if u.String() != "https://b1.s3.sa-east-1.amazonaws.com/" {
		t.Fatal(u)
	}
	u, _ = Creds{Bucket: "b1", Region: "auto", Endpoint: "https://x.r2.cloudflarestorage.com/"}.listURL(nil)
	if u.String() != "https://x.r2.cloudflarestorage.com/b1" {
		t.Fatal(u)
	}
}

func TestTally(t *testing.T) {
	tl := NewTally()
	tl.Add("a/b/1.jpg", 100)
	tl.Add("a/b/2.jpg", 50)
	tl.Add("a/3.pdf", 10)
	tl.Add("a/c/", 0) // marcador de carpeta vacía
	tl.Add("raiz.txt", 5)
	got := map[string]Folder{}
	for _, f := range tl.Folders() {
		got[f.Prefix] = f
	}
	want := map[string]Folder{
		"":     {Prefix: "", Bytes: 165, Objects: 5, FileBytes: 5, Files: 1},
		"a/":   {Prefix: "a/", Bytes: 160, Objects: 4, FileBytes: 10, Files: 1},
		"a/b/": {Prefix: "a/b/", Bytes: 150, Objects: 2, FileBytes: 150, Files: 2},
		"a/c/": {Prefix: "a/c/", Bytes: 0, Objects: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("carpetas: %+v", got)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%q: got %+v want %+v", k, got[k], w)
		}
	}
	if Parent("a/b/") != "a/" || Parent("a/") != "" {
		t.Error("Parent")
	}
}

// Servidor S3 falso: 2.500 objetos en páginas de 1000, exige la firma.
func fakeS3(t *testing.T) *httptest.Server {
	var keys []string
	for i := 0; i < 2500; i++ {
		keys = append(keys, fmt.Sprintf("fotos/%04d.jpg", i))
	}
	keys = append(keys, "docs/2026/informe.pdf", "readme.txt")
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AK/") || r.URL.Path != "/mi-bucket" {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `<Error><Code>AccessDenied</Code><Message>no</Message></Error>`)
			return
		}
		start := 0
		fmt.Sscan(r.URL.Query().Get("continuation-token"), &start)
		max := 1000
		fmt.Sscan(r.URL.Query().Get("max-keys"), &max)
		end := min(start+max, len(keys))
		var b strings.Builder
		b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult>`)
		for _, k := range keys[start:end] {
			size := 1000
			if strings.HasSuffix(k, ".pdf") {
				size = 5_000_000
			}
			fmt.Fprintf(&b, "<Contents><Key>%s</Key><Size>%d</Size></Contents>", k, size)
		}
		if end < len(keys) {
			fmt.Fprintf(&b, "<IsTruncated>true</IsTruncated><NextContinuationToken>%d</NextContinuationToken>", end)
		} else {
			b.WriteString("<IsTruncated>false</IsTruncated>")
		}
		b.WriteString("</ListBucketResult>")
		fmt.Fprint(w, b.String())
	}))
}

func TestScanEndToEnd(t *testing.T) {
	srv := fakeS3(t)
	defer srv.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	box, _ := secret.LoadOrCreate(filepath.Join(t.TempDir(), "k"))
	sc := NewScanner(st, box)

	bad := Creds{Bucket: "mi-bucket", Region: "auto", Endpoint: srv.URL, AccessKey: "otra", SecretKey: "s"}
	if err := sc.Check(context.Background(), bad); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("llaves malas: %v", err)
	}

	id, err := st.CreateBucket(&store.Bucket{Name: "mi-bucket", Bucket: "mi-bucket", Region: "auto", Endpoint: srv.URL,
		AccessKey: "AK", SecretEnc: box.SealString("SK")})
	if err != nil {
		t.Fatal(err)
	}
	if !sc.Start(id) {
		t.Fatal("no inició")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, running := sc.Progress(id); !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no terminó")
		}
		time.Sleep(20 * time.Millisecond)
	}
	b, _ := st.Bucket(id)
	if b.ScanStatus != store.ScanOK || b.TotalObjects != 2502 || b.TotalBytes != 2_500_000+5_000_000+1000 {
		t.Fatalf("bucket: %+v", b)
	}
	subs, total, err := st.BucketSubfolders(id, "", 10)
	if err != nil || total != 2 || subs[0].Prefix != "docs/" || subs[1].Prefix != "fotos/" {
		t.Fatalf("raíz (de mayor a menor): %+v %d %v", subs, total, err)
	}
	if root, _ := st.BucketFolder(id, ""); root.Files != 1 || root.FileBytes != 1000 {
		t.Fatalf("archivos sueltos de la raíz: %+v", root)
	}
	if subs, _, _ := st.BucketSubfolders(id, "docs/", 10); len(subs) != 1 || subs[0].Prefix != "docs/2026/" {
		t.Fatalf("docs/: %+v", subs)
	}
	if err := st.DeleteBucket(id); err != nil {
		t.Fatal(err)
	}
	if subs, total, _ := st.BucketSubfolders(id, "", 10); total != 0 || len(subs) != 0 {
		t.Fatal("las carpetas debían borrarse en cascada")
	}
}
