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

func (s *Store) CreateSession(tokenHash string, userID int64, csrf string, mfaPending bool, expires time.Time) error {
	_, err := s.db.Exec(`INSERT INTO sessions (token_hash, user_id, csrf, mfa_pending, expires_at) VALUES (?, ?, ?, ?, ?)`,
		tokenHash, userID, csrf, boolInt(mfaPending), expires.Unix())
	return err
}

func (s *Store) Session(tokenHash string) (*Session, error) {
	var sess Session
	var pending int
	var exp int64
	err := s.db.QueryRow(`SELECT user_id, csrf, mfa_pending, expires_at FROM sessions WHERE token_hash = ?`, tokenHash).
		Scan(&sess.UserID, &sess.CSRF, &pending, &exp)
	if err != nil {
		return nil, notFound(err)
	}
	sess.MFAPending = pending == 1
	sess.ExpiresAt = time.Unix(exp, 0)
	if time.Now().After(sess.ExpiresAt) {
		s.DeleteSession(tokenHash)
		return nil, ErrNotFound
	}
	return &sess, nil
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
