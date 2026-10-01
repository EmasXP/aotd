package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/EmasXP/aotd/internal/model"
)

const (
	SessionCookie = "aotd_session"
	SessionTTL    = 30 * 24 * time.Hour
	// Sessions are extended at most this often, to avoid a write on every request.
	touchInterval = time.Hour
)

var ErrNoSession = errors.New("auth: no valid session")

type Sessions struct {
	DB *gorm.DB
}

func hashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// Create starts a new session for userID and returns the raw token for the
// cookie. Only the token's SHA-256 is stored.
func (s *Sessions) Create(userID uint) (token string, expires time.Time, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	now := time.Now()
	expires = now.Add(SessionTTL)
	sess := model.Session{UserID: userID, TokenHash: hashToken(token), ExpiresAt: expires, LastSeenAt: now}
	if err := s.DB.Create(&sess).Error; err != nil {
		return "", time.Time{}, err
	}
	return token, expires, nil
}

// Lookup returns the user for token and extends the session (sliding expiry).
// renewed is the new expiry if the session was extended, so the cookie can be
// refreshed too.
func (s *Sessions) Lookup(token string) (user *model.User, renewed time.Time, err error) {
	if token == "" {
		return nil, time.Time{}, ErrNoSession
	}
	var sess model.Session
	err = s.DB.Where("token_hash = ?", hashToken(token)).First(&sess).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, time.Time{}, ErrNoSession
	} else if err != nil {
		return nil, time.Time{}, err
	}
	now := time.Now()
	if now.After(sess.ExpiresAt) {
		s.DB.Delete(&sess)
		return nil, time.Time{}, ErrNoSession
	}
	var u model.User
	if err := s.DB.First(&u, sess.UserID).Error; err != nil {
		return nil, time.Time{}, ErrNoSession
	}
	if now.Sub(sess.LastSeenAt) > touchInterval {
		renewed = now.Add(SessionTTL)
		s.DB.Model(&sess).Updates(map[string]any{"last_seen_at": now, "expires_at": renewed})
	}
	return &u, renewed, nil
}

// Delete ends the session identified by token.
func (s *Sessions) Delete(token string) error {
	return s.DB.Where("token_hash = ?", hashToken(token)).Delete(&model.Session{}).Error
}

// DeleteOthers ends all of userID's sessions except the one for keepToken.
func (s *Sessions) DeleteOthers(userID uint, keepToken string) error {
	return s.DB.Where("user_id = ? AND token_hash <> ?", userID, hashToken(keepToken)).
		Delete(&model.Session{}).Error
}

// PurgeExpired removes expired sessions.
func (s *Sessions) PurgeExpired() error {
	return s.DB.Where("expires_at < ?", time.Now()).Delete(&model.Session{}).Error
}
