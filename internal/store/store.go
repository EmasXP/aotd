// Package store implements AOTD's business rules on top of GORM. Handlers
// call into it; it never deals with HTTP.
package store

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/EmasXP/aotd/internal/cache"
	"github.com/EmasXP/aotd/internal/day"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrForbidden     = errors.New("forbidden")
	ErrAlreadyPosted = errors.New("You've already posted today's AOTD. Come back tomorrow (CET)!")
	ErrUsernameTaken = errors.New("That username is taken.")
	ErrEmailTaken    = errors.New("An account with that email already exists.")
)

// ValidationError is a user-facing input error.
type ValidationError struct{ Msg string }

func (e ValidationError) Error() string { return e.Msg }

func invalid(format string, args ...any) error {
	return ValidationError{Msg: fmt.Sprintf(format, args...)}
}

type Store struct {
	DB    *gorm.DB
	Cache cache.Cache
	Now   func() time.Time // overridable in tests
}

func New(db *gorm.DB) *Store {
	return &Store{DB: db, Cache: cache.Nop{}, Now: time.Now}
}

// userNS holds what's cached about one user.
func (s *Store) userNS(userID uint) cache.Namespace { return cache.NS(s.Cache, "user", userID) }

// invalidateUsers drops what's cached about the users. Call it after the
// change has committed. A failure is only logged: the write itself worked.
func (s *Store) invalidateUsers(userIDs ...uint) {
	for _, id := range userIDs {
		if err := s.userNS(id).Invalidate(); err != nil {
			slog.Warn("cache invalidate", "user", id, "err", err)
		}
	}
}

// Today is the current CET date as YYYY-MM-DD.
func (s *Store) Today() string { return day.Of(s.Now()) }

func notFound(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	return err
}

// likePattern builds a case-insensitive "contains" pattern with LIKE
// wildcards escaped. Use with `LIKE ? ESCAPE '\'`.
func likePattern(q string) string {
	q = strings.ToLower(strings.TrimSpace(q))
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(q) + "%"
}
