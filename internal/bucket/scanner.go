package bucket

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"world/internal/secret"
	"world/internal/store"
)

const (
	// Cada bucket se vuelve a medir solo una vez al día (listar cuesta ~0,005 USD por 1000 peticiones).
	autoEvery   = 24 * time.Hour
	scanTimeout = time.Hour
)

// Scanner mide los buckets en segundo plano, uno a la vez por bucket.
type Scanner struct {
	st   *store.Store
	box  *secret.Box
	http *http.Client

	mu       sync.Mutex
	ctx      context.Context
	progress map[int64]int64     // objetos contados en los escaneos en curso
	started  map[int64]time.Time // último inicio (para no reintentar sin pausa un bucket que falla)
}

func NewScanner(st *store.Store, box *secret.Box) *Scanner {
	return &Scanner{st: st, box: box, http: &http.Client{Timeout: 60 * time.Second},
		ctx: context.Background(), progress: map[int64]int64{}, started: map[int64]time.Time{}}
}

// CredsOf descifra la llave secreta del bucket.
func (s *Scanner) CredsOf(b *store.Bucket) (Creds, error) {
	sec, err := s.box.OpenString(b.SecretEnc)
	if err != nil {
		return Creds{}, errors.New("no se pudo descifrar la llave secreta")
	}
	return Creds{Bucket: b.Bucket, Region: b.Region, Endpoint: b.Endpoint, AccessKey: b.AccessKey, SecretKey: sec}, nil
}

// Check prueba las llaves listando un solo objeto.
func (s *Scanner) Check(ctx context.Context, c Creds) error {
	_, _, err := List(ctx, s.http, c, "", 1)
	return err
}

// Progress: objetos contados hasta ahora (ok=false si el bucket no se está midiendo).
func (s *Scanner) Progress(id int64) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.progress[id]
	return n, ok
}

// Start mide el bucket en segundo plano; false si ya se estaba midiendo.
func (s *Scanner) Start(id int64) bool {
	s.mu.Lock()
	if _, busy := s.progress[id]; busy {
		s.mu.Unlock()
		return false
	}
	s.progress[id] = 0
	s.started[id] = time.Now()
	ctx := s.ctx
	s.mu.Unlock()
	if err := s.st.StartBucketScan(id); err != nil {
		s.done(id)
		return false
	}
	go s.scan(ctx, id)
	return true
}

func (s *Scanner) done(id int64) {
	s.mu.Lock()
	delete(s.progress, id)
	s.mu.Unlock()
}

func (s *Scanner) scan(ctx context.Context, id int64) {
	defer s.done(id)
	ctx, cancel := context.WithTimeout(ctx, scanTimeout)
	defer cancel()
	b, err := s.st.Bucket(id)
	if err != nil {
		return // se eliminó
	}
	c, err := s.CredsOf(b)
	if err != nil {
		s.st.FailBucketScan(id, err.Error())
		return
	}
	start := time.Now()
	t := NewTally()
	var n int64
	token := ""
	for {
		objs, next, err := List(ctx, s.http, c, token, 1000)
		if err != nil {
			if ctx.Err() != nil {
				err = errors.New("se interrumpió (el panel se detuvo o pasó más de 1 hora)")
			}
			s.st.FailBucketScan(id, err.Error())
			return
		}
		for _, o := range objs {
			t.Add(o.Key, o.Size)
		}
		n += int64(len(objs))
		s.mu.Lock()
		s.progress[id] = n
		s.mu.Unlock()
		if next == "" {
			break
		}
		token = next
	}
	folders := make([]store.BucketFolder, 0, len(t.folders))
	for _, f := range t.Folders() {
		folders = append(folders, store.BucketFolder(f))
	}
	root := store.BucketFolder(t.Root())
	if err := s.st.FinishBucketScan(id, folders, root, t.Truncated, time.Since(start)); err != nil {
		log.Printf("bucket %d: guardando escaneo: %v", id, err)
		s.st.FailBucketScan(id, "no se pudo guardar el resultado: "+err.Error())
	}
}

// Run retoma lo interrumpido y vuelve a medir cada bucket una vez al día.
func (s *Scanner) Run(ctx context.Context) {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
	if err := s.st.InterruptedBucketScans(); err != nil {
		log.Printf("buckets: %v", err)
	}
	tick := time.NewTicker(10 * time.Minute)
	defer tick.Stop()
	for {
		if list, err := s.st.Buckets(); err == nil {
			for _, b := range list {
				s.mu.Lock()
				last := s.started[b.ID]
				s.mu.Unlock()
				if !b.Scanning() && time.Since(b.ScannedAt) > autoEvery && time.Since(last) > autoEvery/4 {
					s.Start(b.ID)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
