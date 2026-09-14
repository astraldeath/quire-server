// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"golang.org/x/crypto/argon2"
	"regexp"
	"strings"
	"time"
)

var ErrUnauthorized = errors.New("invalid credentials or expired session")
var ErrInvalid = errors.New("invalid request")
var ErrConflict = errors.New("revision or operation conflict")
var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

func randomID() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func passwordKey(password string, salt []byte) []byte {
	return argon2.IDKey([]byte(password), salt, 3, 64*1024, 1, 32)
}
func passwordValid(password string) bool { return len(password) >= 12 && len(password) <= 1024 }
func (s *Store) CreateUser(username, password string) error {
	if !usernamePattern.MatchString(username) || !passwordValid(password) {
		return ErrInvalid
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	key := passwordKey(password, salt)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	id := randomID()
	if _, err = tx.Exec("INSERT INTO users (id,username,salt,password_hash) VALUES (?,?,?,?)", id, username, salt, key); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO cursors VALUES (?,0)", id); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) ResetPassword(username, password string) error {
	if !passwordValid(password) {
		return ErrInvalid
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	key := passwordKey(password, salt)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec("UPDATE users SET salt=?,password_hash=? WHERE username=?", salt, key, username)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	if _, err = tx.Exec("DELETE FROM sessions WHERE user_id=(SELECT id FROM users WHERE username=?)", username); err != nil {
		return err
	}
	return tx.Commit()
}

type Session struct {
	ID         string `json:"id"`
	DeviceName string `json:"deviceName"`
	CreatedAt  int64  `json:"createdAt"`
	ExpiresAt  int64  `json:"expiresAt"`
}
type LoginResult struct {
	Token   string  `json:"token"`
	Session Session `json:"session"`
}

func (s *Store) Login(username, password, device string) (LoginResult, error) {
	if len(password) > 1024 || len(username) > 64 || len(strings.TrimSpace(device)) == 0 || len(device) > 100 {
		return LoginResult{}, ErrInvalid
	}
	// Keep verification and session creation serialized with owner password resets.
	tx, err := s.db.Begin()
	if err != nil {
		return LoginResult{}, err
	}
	defer tx.Rollback()
	var user string
	var salt, key []byte
	err = tx.QueryRow("SELECT id,salt,password_hash FROM users WHERE username=? AND disabled=0", username).Scan(&user, &salt, &key)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return LoginResult{}, err
	}
	missing := errors.Is(err, sql.ErrNoRows)
	if missing {
		salt = make([]byte, 16)
		key = make([]byte, 32)
	}
	derived := passwordKey(password, salt)
	if subtle.ConstantTimeCompare(derived, key) != 1 || missing {
		return LoginResult{}, ErrUnauthorized
	}
	token := randomID()
	hash := sha256.Sum256([]byte(token))
	now := time.Now().Unix()
	session := Session{randomID(), strings.TrimSpace(device), now, now + int64((30 * 24 * time.Hour).Seconds())}
	if _, err = tx.Exec("DELETE FROM sessions WHERE expires_at<=?", now); err != nil {
		return LoginResult{}, err
	}
	var count int
	if err = tx.QueryRow("SELECT count(*) FROM sessions WHERE user_id=?", user).Scan(&count); err != nil {
		return LoginResult{}, err
	}
	if count >= 32 {
		return LoginResult{}, ErrConflict
	}
	if _, err = tx.Exec("INSERT INTO sessions VALUES (?,?,?,?,?,?)", session.ID, user, hash[:], session.DeviceName, now, session.ExpiresAt); err != nil {
		return LoginResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return LoginResult{}, err
	}
	return LoginResult{token, session}, nil
}
func (s *Store) authenticate(token string) (string, error) {
	if len(token) != 64 {
		return "", ErrUnauthorized
	}
	hash := sha256.Sum256([]byte(token))
	var user string
	err := s.db.QueryRow("SELECT user_id FROM sessions JOIN users ON users.id=sessions.user_id WHERE token_hash=? AND expires_at>? AND users.disabled=0", hash[:], time.Now().Unix()).Scan(&user)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrUnauthorized
	}
	return user, err
}
func (s *Store) sessions(user string) ([]Session, error) {
	rows, err := s.db.Query("SELECT id,device_name,created_at,expires_at FROM sessions WHERE user_id=? AND expires_at>? ORDER BY created_at,id", user, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Session{}
	for rows.Next() {
		var v Session
		if err = rows.Scan(&v.ID, &v.DeviceName, &v.CreatedAt, &v.ExpiresAt); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}
func (s *Store) revoke(user, id string) error {
	result, err := s.db.Exec("DELETE FROM sessions WHERE user_id=? AND id=?", user, id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
