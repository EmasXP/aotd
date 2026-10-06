// Package store implements AOTD's business rules on top of GORM. Handlers
// call into it; it never deals with HTTP.
package store

import (
	"errors"
	"fmt"
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
	Cache cache.Cache      // see cached.go
	Now   func() time.Time // overridable in tests
}

func New(db *gorm.DB) *Store {
	return &Store{DB: db, Cache: cache.Nop{}, Now: time.Now}
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
