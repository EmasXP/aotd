package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"time"

	"gorm.io/gorm"

	"github.com/EmasXP/aotd/internal/cache"
	"github.com/EmasXP/aotd/internal/model"
)

const (
	SessionCookie = "aotd_session"
	SessionTTL    = 30 * 24 * time.Hour
	// Sessions are extended at most this often, to avoid a write on every request.
	touchInterval = time.Hour
	// cacheTTL is short as a safety net: a missed invalidation must not keep
	// a deleted session alive for long.
	cacheTTL = 5 * time.Minute
)

var ErrNoSession = errors.New("auth: no valid session")

// Sessions are cached per user (namespace sessions:{userID}) so ending all
// of a user's sessions is one invalidation. The token hash to user ID
// mapping never changes, so it's a plain key.
type Sessions struct {
	DB    *gorm.DB
	Cache cache.Cache // nil caches nothing
}

func (s *Sessions) cache() cache.Cache {
	if s.Cache == nil {
		return cache.Nop{}
	}
	return s.Cache
}

func (s *Sessions) invalidate(userID uint) {
	if err := cache.NS(s.cache(), "sessions", userID).Invalidate(); err != nil {
		slog.Warn("cache invalidate sessions", "user", userID, "err", err)
	}
}

func (s *Sessions) byHash(h []byte) (model.Session, error) {
	var sess model.Session
	err := s.DB.Where("token_hash = ?", h).First(&sess).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return sess, ErrNoSession
	}
	return sess, err
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

// Lookup returns the user ID for token and extends the session (sliding
// expiry). renewed is the new expiry if the session was extended, so the
// cookie can be refreshed too.
func (s *Sessions) Lookup(token string) (userID uint, renewed time.Time, err error) {
	if token == "" {
		return 0, time.Time{}, ErrNoSession
	}
	h := hashToken(token)
	key := hex.EncodeToString(h)
	userID, err = cache.FetchKey(s.cache(), "token:"+key, cacheTTL, func() (uint, error) {
		sess, err := s.byHash(h)
		return sess.UserID, err
	})
	if err != nil {
		return 0, time.Time{}, err
	}
	sess, err := cache.Fetch(cache.NS(s.cache(), "sessions", userID), key, cacheTTL, func() (model.Session, error) {
		return s.byHash(h)
	})
	if err != nil {
		return 0, time.Time{}, err
	}
	now := time.Now()
	if now.After(sess.ExpiresAt) {
		// No invalidation needed: a cached copy is just as expired.
		s.DB.Delete(&model.Session{}, sess.ID)
		return 0, time.Time{}, ErrNoSession
	}
	if now.Sub(sess.LastSeenAt) > touchInterval {
		renewed = now.Add(SessionTTL)
		s.DB.Model(&model.Session{}).Where("id = ?", sess.ID).Updates(map[string]any{"last_seen_at": now, "expires_at": renewed})
		s.invalidate(userID)
	}
	return userID, renewed, nil
}

// Delete ends the session identified by token.
func (s *Sessions) Delete(token string) error {
	sess, err := s.byHash(hashToken(token))
	if errors.Is(err, ErrNoSession) {
		return nil
	} else if err != nil {
		return err
	}
	if err := s.DB.Delete(&sess).Error; err != nil {
		return err
	}
	s.invalidate(sess.UserID)
	return nil
}

// DeleteOthers ends all of userID's sessions except the one for keepToken.
func (s *Sessions) DeleteOthers(userID uint, keepToken string) error {
	err := s.DB.Where("user_id = ? AND token_hash <> ?", userID, hashToken(keepToken)).
		Delete(&model.Session{}).Error
	if err != nil {
		return err
	}
	s.invalidate(userID)
	return nil
}

// PurgeExpired removes expired sessions. Cached ones need no invalidation:
// Lookup checks the expiry itself.
func (s *Sessions) PurgeExpired() error {
	return s.DB.Where("expires_at < ?", time.Now()).Delete(&model.Session{}).Error
}
