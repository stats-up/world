package store

import "time"

// Resoluciones de las métricas guardadas.
const (
	ResMinute  = 60  // detalle: se guarda 48 h
	ResQuarter = 900 // resumen de 15 min: se guarda 30 días
)

const (
	keepMinute  = 48 * time.Hour
	keepQuarter = 30 * 24 * time.Hour
)

// HostPoint es una muestra del servidor. CPU en % del total (0–100).
type HostPoint struct {
	TS                  int64
	CPU                 float64
	MemUsed, MemTotal   int64
	SwapUsed, SwapTotal int64
	DiskUsed, DiskTotal int64
	Load1               float64
}

// CtrPoint es una muestra de un sitio o servicio (suma de sus contenedores).
// CPU en núcleos usados (1.0 = una vCPU completa); red y disco en bytes por segundo.
type CtrPoint struct {
	Key          string
	TS           int64
	CPU          float64
	Mem          int64
	MemLimit     int64
	NetRx, NetTx float64
	BlkR, BlkW   float64
	Restarts     int64
	OOMKills     int64 // procesos terminados por falta de memoria desde que partió el contenedor
}

// SaveHostMetric guarda la muestra en el minuto que le corresponde; si ya hay una en ese minuto,
// promedia la CPU y conserva el máximo de memoria (los picos de RAM son lo que importa).
func (s *Store) SaveHostMetric(p HostPoint) error {
	_, err := s.db.Exec(`INSERT INTO metrics_host (res, ts, cpu, mem_used, mem_total, swap_used, swap_total, disk_used, disk_total, load1)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (res, ts) DO UPDATE SET
			cpu = (cpu + excluded.cpu) / 2, mem_used = max(mem_used, excluded.mem_used), mem_total = excluded.mem_total,
			swap_used = max(swap_used, excluded.swap_used), swap_total = excluded.swap_total,
			disk_used = excluded.disk_used, disk_total = excluded.disk_total, load1 = max(load1, excluded.load1)`,
		ResMinute, p.TS/ResMinute*ResMinute, p.CPU, p.MemUsed, p.MemTotal, p.SwapUsed, p.SwapTotal, p.DiskUsed, p.DiskTotal, p.Load1)
	return err
}

// SaveCtrMetrics guarda las muestras de los contenedores con la misma lógica que SaveHostMetric.
func (s *Store) SaveCtrMetrics(points []CtrPoint) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, p := range points {
		_, err := tx.Exec(`INSERT INTO metrics_ctr (key, res, ts, cpu, mem, mem_limit, net_rx, net_tx, blk_r, blk_w, restarts, oom)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (key, res, ts) DO UPDATE SET
				cpu = (cpu + excluded.cpu) / 2, mem = max(mem, excluded.mem), mem_limit = excluded.mem_limit,
				net_rx = (net_rx + excluded.net_rx) / 2, net_tx = (net_tx + excluded.net_tx) / 2,
				blk_r = (blk_r + excluded.blk_r) / 2, blk_w = (blk_w + excluded.blk_w) / 2,
				restarts = max(restarts, excluded.restarts), oom = max(oom, excluded.oom)`,
			p.Key, ResMinute, p.TS/ResMinute*ResMinute, p.CPU, p.Mem, p.MemLimit, p.NetRx, p.NetTx, p.BlkR, p.BlkW, p.Restarts, p.OOMKills)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RollupMetrics resume en bloques de 15 min el detalle por minuto del intervalo [from, to)
// (from y to alineados a 15 min) y borra lo que ya venció.
func (s *Store) RollupMetrics(from, to int64, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmts := []string{
		`INSERT OR REPLACE INTO metrics_host (res, ts, cpu, mem_used, mem_total, swap_used, swap_total, disk_used, disk_total, load1)
			SELECT ?, ts / ? * ?, avg(cpu), max(mem_used), max(mem_total), max(swap_used), max(swap_total), max(disk_used), max(disk_total), max(load1)
			FROM metrics_host WHERE res = ? AND ts >= ? AND ts < ? GROUP BY ts / ?`,
		`INSERT OR REPLACE INTO metrics_ctr (key, res, ts, cpu, mem, mem_limit, net_rx, net_tx, blk_r, blk_w, restarts, oom)
			SELECT key, ?, ts / ? * ?, avg(cpu), max(mem), max(mem_limit), avg(net_rx), avg(net_tx), avg(blk_r), avg(blk_w), max(restarts), max(oom)
			FROM metrics_ctr WHERE res = ? AND ts >= ? AND ts < ? GROUP BY key, ts / ?`,
	}
	for _, q := range stmts {
		if _, err := tx.Exec(q, ResQuarter, ResQuarter, ResQuarter, ResMinute, from, to, ResQuarter); err != nil {
			return err
		}
	}
	for _, t := range []string{"metrics_host", "metrics_ctr"} {
		if _, err := tx.Exec(`DELETE FROM `+t+` WHERE (res = ? AND ts < ?) OR (res = ? AND ts < ?)`,
			ResMinute, now.Add(-keepMinute).Unix(), ResQuarter, now.Add(-keepQuarter).Unix()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// HostSeries devuelve las muestras del servidor desde `since`, en orden.
func (s *Store) HostSeries(res int, since int64) ([]HostPoint, error) {
	rows, err := s.db.Query(`SELECT ts, cpu, mem_used, mem_total, swap_used, swap_total, disk_used, disk_total, load1
		FROM metrics_host WHERE res = ? AND ts >= ? ORDER BY ts`, res, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HostPoint
	for rows.Next() {
		var p HostPoint
		if err := rows.Scan(&p.TS, &p.CPU, &p.MemUsed, &p.MemTotal, &p.SwapUsed, &p.SwapTotal, &p.DiskUsed, &p.DiskTotal, &p.Load1); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CtrSeries devuelve las series de los sitios/servicios desde `since`, agrupadas por clave.
// Con key vacía trae todas.
func (s *Store) CtrSeries(key string, res int, since int64) (map[string][]CtrPoint, error) {
	q := `SELECT key, ts, cpu, mem, mem_limit, net_rx, net_tx, blk_r, blk_w, restarts, oom
		FROM metrics_ctr WHERE res = ? AND ts >= ?`
	args := []any{res, since}
	if key != "" {
		q += ` AND key = ?`
		args = append(args, key)
	}
	rows, err := s.db.Query(q+` ORDER BY key, ts`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]CtrPoint{}
	for rows.Next() {
		var p CtrPoint
		if err := rows.Scan(&p.Key, &p.TS, &p.CPU, &p.Mem, &p.MemLimit, &p.NetRx, &p.NetTx, &p.BlkR, &p.BlkW, &p.Restarts, &p.OOMKills); err != nil {
			return nil, err
		}
		out[p.Key] = append(out[p.Key], p)
	}
	return out, rows.Err()
}

// DeploymentTimes devuelve el inicio de los deploys del sitio desde `since` (para marcarlos en los gráficos).
func (s *Store) DeploymentTimes(siteID, since int64) ([]int64, error) {
	rows, err := s.db.Query(`SELECT started_at FROM deployments WHERE site_id = ? AND started_at >= ? ORDER BY started_at`, siteID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var t int64
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
