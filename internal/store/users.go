package store

import (
	"time"
)

type User struct {
	ID           int64
	Email        string
	PasswordHash string
	TOTPSecret   string
	TOTPEnabled  bool
}

type Session struct {
	UserID     int64
	CSRF       string
	MFAPending bool
	ExpiresAt  time.Time
	IP         string
	UserAgent  string
	CreatedAt  time.Time
	LastSeen   time.Time
}

const userCols = `id, email, password_hash, totp_secret, totp_enabled`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var totp int
	if err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.TOTPSecret, &totp); err != nil {
		return nil, notFound(err)
	}
	u.TOTPEnabled = totp == 1
	return &u, nil
}

func (s *Store) CountUsers() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

func (s *Store) CreateUser(email, passwordHash string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO users (email, password_hash, created_at) VALUES (?, ?, ?)`,
		email, passwordHash, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UserByID(id int64) (*User, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

func (s *Store) UserByEmail(email string) (*User, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE email = ? COLLATE NOCASE`, email))
}

func (s *Store) SetPassword(userID int64, hash string) error {
	_, err := s.db.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, hash, userID)
	return err
}

func (s *Store) SetTOTP(userID int64, secret string, enabled bool) error {
	_, err := s.db.Exec(`UPDATE users SET totp_secret = ?, totp_enabled = ? WHERE id = ?`,
		secret, boolInt(enabled), userID)
	return err
}

// CreateSession guarda una sesión nueva; ip y userAgent son de quien inició sesión.
func (s *Store) CreateSession(tokenHash string, userID int64, csrf string, mfaPending bool, expires time.Time, ip, userAgent string) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`INSERT INTO sessions (token_hash, user_id, csrf, mfa_pending, expires_at, ip, user_agent, created_at, last_seen)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		tokenHash, userID, csrf, boolInt(mfaPending), expires.Unix(), ip, userAgent, now, now)
	return err
}

const sessionCols = `user_id, csrf, mfa_pending, expires_at, ip, user_agent, created_at, last_seen`

func scanSession(row interface{ Scan(...any) error }, extra ...any) (*Session, error) {
	var sess Session
	var pending int
	var exp, created, seen int64
	dest := append([]any{&sess.UserID, &sess.CSRF, &pending, &exp, &sess.IP, &sess.UserAgent, &created, &seen}, extra...)
	if err := row.Scan(dest...); err != nil {
		return nil, notFound(err)
	}
	sess.MFAPending = pending == 1
	sess.ExpiresAt = time.Unix(exp, 0)
	if created > 0 {
		sess.CreatedAt = time.Unix(created, 0)
	}
	if seen > 0 {
		sess.LastSeen = time.Unix(seen, 0)
	}
	return &sess, nil
}

func (s *Store) Session(tokenHash string) (*Session, error) {
	sess, err := scanSession(s.db.QueryRow(`SELECT `+sessionCols+` FROM sessions WHERE token_hash = ?`, tokenHash))
	if err != nil {
		return nil, err
	}
	if time.Now().After(sess.ExpiresAt) {
		s.DeleteSession(tokenHash)
		return nil, ErrNotFound
	}
	return sess, nil
}

// UserSession es una sesión con su identificador (el hash del token, nunca el token).
type UserSession struct {
	Hash string
	*Session
}

// UserSessions: sesiones vigentes del usuario, la de uso más reciente primero.
func (s *Store) UserSessions(userID int64) ([]UserSession, error) {
	rows, err := s.db.Query(`SELECT `+sessionCols+`, token_hash FROM sessions WHERE user_id = ? AND expires_at > ?
		ORDER BY last_seen DESC, created_at DESC`, userID, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserSession
	for rows.Next() {
		var hash string
		sess, err := scanSession(rows, &hash)
		if err != nil {
			return nil, err
		}
		out = append(out, UserSession{Hash: hash, Session: sess})
	}
	return out, rows.Err()
}

// TouchSession anota el último uso de la sesión.
func (s *Store) TouchSession(tokenHash string, at time.Time) error {
	_, err := s.db.Exec(`UPDATE sessions SET last_seen = ? WHERE token_hash = ?`, at.Unix(), tokenHash)
	return err
}

// DeleteUserSession cierra una sesión solo si es del usuario indicado.
func (s *Store) DeleteUserSession(userID int64, tokenHash string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM sessions WHERE user_id = ? AND token_hash = ?`, userID, tokenHash)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) CompleteMFA(tokenHash string) error {
	_, err := s.db.Exec(`UPDATE sessions SET mfa_pending = 0 WHERE token_hash = ?`, tokenHash)
	return err
}

func (s *Store) DeleteSession(tokenHash string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

// DeleteUserSessions cierra todas las sesiones de un usuario salvo la indicada (puede ser "").
func (s *Store) DeleteUserSessions(userID int64, except string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE user_id = ? AND token_hash != ?`, userID, except)
	return err
}

func (s *Store) PurgeExpiredSessions() error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, time.Now().Unix())
	return err
}
