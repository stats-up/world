package deploy

import (
	"context"
	"testing"
	"time"
)

func TestCronHold(t *testing.T) {
	e := &Engine{busy: map[int64]bool{}}
	c := NewCron(e, nil, time.Minute)
	if e.cron != c {
		t.Fatal("NewCron debe quedar asociado al engine")
	}
	ctx := context.Background()

	// Sin ejecución en curso: pausa al tiro.
	release, waited, err := c.hold(ctx, 1, time.Second)
	if err != nil || waited {
		t.Fatalf("hold libre: waited=%v err=%v", waited, err)
	}
	if !c.held[1] {
		t.Fatal("el sitio debe quedar en pausa")
	}
	release()
	if c.held[1] {
		t.Fatal("release debe reanudar el cron")
	}

	// Con una ejecución en curso: espera a que termine.
	c.running[2] = true
	go func() {
		time.Sleep(1500 * time.Millisecond)
		c.mu.Lock()
		delete(c.running, 2)
		c.mu.Unlock()
	}()
	start := time.Now()
	release, waited, err = c.hold(ctx, 2, 10*time.Second)
	if err != nil || !waited || time.Since(start) < time.Second {
		t.Fatalf("debió esperar: waited=%v err=%v %s", waited, err, time.Since(start))
	}
	release()

	// Si no termina a tiempo: error y el cron no queda en pausa.
	c.running[3] = true
	if _, _, err := c.hold(ctx, 3, 1500*time.Millisecond); err == nil {
		t.Fatal("debió fallar por tiempo")
	}
	if c.held[3] {
		t.Fatal("tras el error el cron no debe quedar en pausa")
	}
}
