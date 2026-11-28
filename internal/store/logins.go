package store

import (
	"time"
)

// Resultados de un intento de acceso al panel.
const (
	LoginOK          = "ok"       // contraseña (y 2FA si corresponde) correctos
	LoginBadPassword = "password" // correo o contraseña incorrectos
	LoginBad2FA      = "2fa"      // contraseña correcta, código 2FA incorrecto
	LoginLocked      = "locked"   // demasiados intentos: se rechazó sin revisar la contraseña
)

// Cuánto se guarda el historial de accesos.
const (
	LoginEventsMaxAge  = 180 * 24 * time.Hour
	LoginEventsMaxRows = 20000
)

// LoginEvent es un intento de acceso al panel.
type LoginEvent struct {
	ID          int64
	At          time.Time
	UserID      int64
	Email       string
	IP          string
	IPKey       string // la IP, o su red /64 si es IPv6 (los celulares cambian la dirección dentro de su red)
	UserAgent   string
	Result      string
	NewIP       bool   // acceso correcto desde una IP con la que esta cuenta nunca había entrado
	SessionHash string // sesión creada en el acceso (o la que esperaba el 2FA)
	Ack         bool   // el usuario confirmó que fue él
}

func (e *LoginEvent) OK() bool { return e.Result == LoginOK }

// AddLoginEvent guarda el intento. En un acceso correcto marca NewIP si la cuenta ya había entrado
// antes desde otras IPs pero nunca desde esta (el primer acceso de todos no se marca).
func (s *Store) AddLoginEvent(e *LoginEvent) error {
	if e.Result == LoginOK && e.UserID > 0 {
		var known, any int
		s.db.QueryRow(`SELECT COUNT(*) FROM login_events WHERE user_id = ? AND result = 'ok' AND ip_key = ?`, e.UserID, e.IPKey).Scan(&known)
		s.db.QueryRow(`SELECT COUNT(*) FROM login_events WHERE user_id = ? AND result = 'ok'`, e.UserID).Scan(&any)
		e.NewIP = known == 0 && any > 0
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	res, err := s.db.Exec(`INSERT INTO login_events (at, user_id, email, ip, ip_key, user_agent, result, new_ip, session_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.At.Unix(), e.UserID, e.Email, e.IP, e.IPKey, e.UserAgent, e.Result, boolInt(e.NewIP), e.SessionHash)
	if err != nil {
		return err
	}
	e.ID, _ = res.LastInsertId()
	return nil
}

// KnownLoginIP indica si la cuenta ya entró correctamente desde esa IP (o su red /64).
func (s *Store) KnownLoginIP(userID int64, ipKey string) bool {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM login_events WHERE user_id = ? AND result = 'ok' AND ip_key = ?`, userID, ipKey).Scan(&n)
	return n > 0
}

const loginCols = `id, at, user_id, email, ip, ip_key, user_agent, result, new_ip, session_hash, ack`

func scanLogin(row interface{ Scan(...any) error }) (*LoginEvent, error) {
	var e LoginEvent
	var at int64
	var newIP, ack int
	if err := row.Scan(&e.ID, &at, &e.UserID, &e.Email, &e.IP, &e.IPKey, &e.UserAgent, &e.Result, &newIP, &e.SessionHash, &ack); err != nil {
		return nil, notFound(err)
	}
	e.At, e.NewIP, e.Ack = time.Unix(at, 0), newIP == 1, ack == 1
	return &e, nil
}

func (s *Store) queryLogins(q string, args ...any) ([]*LoginEvent, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*LoginEvent
	for rows.Next() {
		e, err := scanLogin(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// LoginEvents: los últimos intentos de acceso (de todas las cuentas).
func (s *Store) LoginEvents(limit int) ([]*LoginEvent, error) {
	return s.queryLogins(`SELECT `+loginCols+` FROM login_events ORDER BY at DESC, id DESC LIMIT ?`, limit)
}

// LoginAlerts: accesos de la cuenta que debería revisar quien la usa: entradas desde una IP nueva y
// contraseñas correctas con el 2FA fallido. No incluye los de la sesión actual (ya sabe que es él).
func (s *Store) LoginAlerts(userID int64, currentSession string) ([]*LoginEvent, error) {
	return s.queryLogins(`SELECT `+loginCols+` FROM login_events WHERE user_id = ? AND ack = 0 AND session_hash != ?
		AND ((result = 'ok' AND new_ip = 1) OR result = '2fa') AND at > ? ORDER BY at DESC LIMIT 10`,
		userID, currentSession, time.Now().Add(-30*24*time.Hour).Unix())
}

// AckLoginAlert marca la alerta como revisada (solo si es de la cuenta).
func (s *Store) AckLoginAlert(userID, id int64) error {
	_, err := s.db.Exec(`UPDATE login_events SET ack = 1 WHERE id = ? AND user_id = ?`, id, userID)
	return err
}

// AckAllLoginAlerts marca como revisadas todas las alertas de la cuenta.
func (s *Store) AckAllLoginAlerts(userID int64) error {
	_, err := s.db.Exec(`UPDATE login_events SET ack = 1 WHERE user_id = ? AND ack = 0`, userID)
	return err
}

// PurgeLoginEvents borra el historial antiguo y lo limita a LoginEventsMaxRows filas.
func (s *Store) PurgeLoginEvents() error {
	if _, err := s.db.Exec(`DELETE FROM login_events WHERE at < ?`, time.Now().Add(-LoginEventsMaxAge).Unix()); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM login_events WHERE id <= (SELECT id FROM login_events ORDER BY id DESC LIMIT 1 OFFSET ?)`, LoginEventsMaxRows)
	return err
}
