package dbmigrate

import (
	"context"
	"encoding/json"
	"regexp"
	"sync"
	"time"
)

// TableStat es el tamaño aproximado de una tabla del origen.
type TableStat struct {
	Rows  int64 `json:"r"`
	Bytes int64 `json:"b"`
}

// EncodeTableStats y DecodeTableStats guardan las estimaciones del origen junto a la migración.
func EncodeTableStats(m map[string]TableStat) string {
	if len(m) == 0 {
		return ""
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func DecodeTableStats(s string) map[string]TableStat {
	m := map[string]TableStat{}
	json.Unmarshal([]byte(s), &m)
	return m
}

// Live es lo que se mide mientras la copia corre (no se guarda).
type Live struct {
	Table    string        // tabla que se está copiando
	Rows     int64         // filas que ya tiene en MainDB (aprox.)
	RowsEst  int64         // filas en el origen (aprox.)
	Received int64         // bytes recibidos del origen
	Rate     float64       // bytes por segundo en el último minuto
	Idle     time.Duration // tiempo sin recibir datos
	DBState  string        // qué hace MainDB con la instrucción en curso
	DBTime   int           // segundos que lleva esa instrucción
}

func (l *Live) RowsPercent() int {
	if l.RowsEst <= 0 {
		return 0
	}
	return int(min(100, l.Rows*100/l.RowsEst))
}

// Stalled: más de dos minutos sin datos nuevos.
func (l *Live) Stalled() bool { return l.Idle >= 2*time.Minute }

func (l *Live) RatePerSec() int64 { return int64(l.Rate) }

// liveTracker acumula las muestras de red del contenedor.
type liveTracker struct {
	mu      sync.Mutex
	live    Live
	samples []sample
	moved   time.Time
}

type sample struct {
	at time.Time
	rx int64
}

func (t *liveTracker) addRx(now time.Time, rx int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if rx != t.live.Received || t.moved.IsZero() {
		t.moved = now
	}
	t.live.Received = rx
	t.live.Idle = now.Sub(t.moved).Round(time.Second)
	t.samples = append(t.samples, sample{now, rx})
	for len(t.samples) > 2 && now.Sub(t.samples[0].at) > time.Minute {
		t.samples = t.samples[1:]
	}
	if first := t.samples[0]; now.After(first.at) {
		t.live.Rate = float64(rx-first.rx) / now.Sub(first.at).Seconds()
	}
}

func (t *liveTracker) set(f func(l *Live)) {
	t.mu.Lock()
	f(&t.live)
	t.mu.Unlock()
}

func (t *liveTracker) get() Live {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.live
}

var tableNameRe = regexp.MustCompile("(?m)^-- Table structure for table `([^`]+)`")

// CurrentTable es la última tabla que el dump empezó a copiar.
func CurrentTable(log string) string {
	all := tableNameRe.FindAllStringSubmatch(log, -1)
	if len(all) == 0 {
		return ""
	}
	return all[len(all)-1][1]
}

// targetProgress lee en MainDB las filas de la tabla en curso y qué está haciendo el servidor.
func targetProgress(ctx context.Context, c Conn, db, table string) (rows int64, state string, secs int, err error) {
	c.Database = ""
	pool, err := open(ctx, c)
	if err != nil {
		return 0, "", 0, err
	}
	defer pool.Close()
	conn, err := pool.Conn(ctx)
	if err != nil {
		return 0, "", 0, err
	}
	defer conn.Close()
	// MySQL 8 guarda en caché las estadísticas de information_schema (24 h por defecto).
	conn.ExecContext(ctx, `SET SESSION information_schema_stats_expiry = 0`)
	if table != "" {
		conn.QueryRowContext(ctx, `SELECT COALESCE(table_rows, 0) FROM information_schema.tables WHERE table_schema = ? AND table_name = ?`,
			db, table).Scan(&rows)
	}
	conn.QueryRowContext(ctx, `SELECT COALESCE(state, ''), time FROM information_schema.processlist
		WHERE db = ? AND command = 'Query' ORDER BY time DESC LIMIT 1`, db).Scan(&state, &secs)
	return rows, state, secs, nil
}
