package auth

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/EmasXP/aotd/internal/cache"
	"github.com/EmasXP/aotd/internal/db"
	"github.com/EmasXP/aotd/internal/model"
)

// newSessions uses a real cache, so a missed invalidation shows as a
// session that's still valid.
func newSessions(t *testing.T) *Sessions {
	t.Helper()
	g, err := db.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name()))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(g); err != nil {
		t.Fatal(err)
	}
	c, err := cache.OpenBolt(filepath.Join(t.TempDir(), "cache.db"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &Sessions{DB: g, Cache: c}
}

func TestSessionDelete(t *testing.T) {
	s := newSessions(t)
	tok, _, _ := s.Create(1)
	if uid, _, err := s.Lookup(tok); err != nil || uid != 1 {
		t.Fatalf("Lookup = %d, %v", uid, err)
	}
	if err := s.Delete(tok); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Lookup(tok); !errors.Is(err, ErrNoSession) {
		t.Errorf("after Delete: %v", err)
	}
	if _, _, err := s.Lookup("made-up"); !errors.Is(err, ErrNoSession) {
		t.Errorf("unknown token: %v", err)
	}
}

func TestSessionDeleteOthers(t *testing.T) {
	s := newSessions(t)
	keep, _, _ := s.Create(1)
	other, _, _ := s.Create(1)
	someoneElse, _, _ := s.Create(2)
	for _, tok := range []string{keep, other, someoneElse} {
		s.Lookup(tok)
	}
	if err := s.DeleteOthers(1, keep); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Lookup(other); !errors.Is(err, ErrNoSession) {
		t.Errorf("other session: %v", err)
	}
	if _, _, err := s.Lookup(keep); err != nil {
		t.Errorf("kept session: %v", err)
	}
	if _, _, err := s.Lookup(someoneElse); err != nil {
		t.Errorf("another user's session: %v", err)
	}
}

func TestSessionTouch(t *testing.T) {
	s := newSessions(t)
	tok, _, _ := s.Create(1)
	s.DB.Model(&model.Session{}).Where("user_id = 1").Update("last_seen_at", time.Now().Add(-2*touchInterval))
	if _, renewed, _ := s.Lookup(tok); renewed.IsZero() {
		t.Fatal("not renewed")
	}
	// Renewed once; the cache must not keep the old last-seen time.
	if _, renewed, _ := s.Lookup(tok); !renewed.IsZero() {
		t.Error("renewed again")
	}
}

func TestSessionExpired(t *testing.T) {
	s := newSessions(t)
	tok, _, _ := s.Create(1)
	s.DB.Model(&model.Session{}).Where("user_id = 1").Update("expires_at", time.Now().Add(-time.Minute))
	if _, _, err := s.Lookup(tok); !errors.Is(err, ErrNoSession) {
		t.Errorf("expired: %v", err)
	}
	var n int64
	if s.DB.Model(&model.Session{}).Count(&n); n != 0 {
		t.Errorf("expired session not deleted")
	}
}
