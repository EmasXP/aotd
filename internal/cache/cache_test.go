package cache

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func newBolt(t *testing.T) (*Bolt, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cache.db")
	c, err := OpenBolt(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, path
}

func TestBoltGetSetDelete(t *testing.T) {
	c, _ := newBolt(t)
	if _, ok, err := c.Get("a"); ok || err != nil {
		t.Fatalf("empty Get = %v, %v", ok, err)
	}
	c.Set("a", []byte("1"), time.Minute)
	c.Set("b", []byte("2"), 0)
	if v, ok, _ := c.Get("a"); !ok || string(v) != "1" {
		t.Errorf("Get a = %q, %v", v, ok)
	}
	c.Delete("a", "missing")
	if _, ok, _ := c.Get("a"); ok {
		t.Error("deleted key still there")
	}
	if v, ok, _ := c.Get("b"); !ok || string(v) != "2" {
		t.Errorf("Get b = %q, %v", v, ok)
	}
}

func TestBoltExpiryAndSweep(t *testing.T) {
	c, _ := newBolt(t)
	now := time.Now()
	c.now = func() time.Time { return now }
	// Enough keys for several sweep batches; even ones expire.
	const n = sweepBatch*2 + 500
	for i := range n {
		ttl := time.Hour
		if i%2 == 0 {
			ttl = time.Minute
		}
		c.Set(fmt.Sprintf("k%05d", i), []byte("x"), ttl)
	}
	c.Set("forever", []byte("x"), 0)
	now = now.Add(2 * time.Minute)

	if _, ok, _ := c.Get("k00000"); ok {
		t.Error("expired key served")
	}
	if err := c.sweep(); err != nil {
		t.Fatal(err)
	}
	var left int
	c.db.View(func(tx *bolt.Tx) error {
		left = tx.Bucket(bucket).Stats().KeyN
		return nil
	})
	if want := n/2 + 1; left != want {
		t.Errorf("keys after sweep = %d, want %d", left, want)
	}
}

func TestBoltStartsEmpty(t *testing.T) {
	c, path := newBolt(t)
	c.Set("a", []byte("1"), 0)
	c.Close()
	c2, err := OpenBolt(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if _, ok, _ := c2.Get("a"); ok {
		t.Error("old cache survived reopen")
	}
}

func TestNamespaceInvalidate(t *testing.T) {
	c, _ := newBolt(t)
	loads := 0
	load := func() (int, error) { loads++; return loads, nil }
	alice, bob := NS(c, "user", 1), NS(c, "user", 2)

	for range 2 {
		if v, _ := Fetch(alice, "n", time.Minute, load); v != 1 {
			t.Errorf("alice = %d, want 1", v)
		}
	}
	Fetch(bob, "n", time.Minute, load) // loads = 2
	alice.Invalidate()
	if v, _ := Fetch(alice, "n", time.Minute, load); v != 3 {
		t.Errorf("alice after invalidate = %d, want 3", v)
	}
	if v, _ := Fetch(bob, "n", time.Minute, load); v != 2 {
		t.Errorf("bob = %d, want 2 (other namespace untouched)", v)
	}
}

func TestFetchDuringInvalidate(t *testing.T) {
	c, _ := newBolt(t)
	ns := NS(c, "user", 1)
	// The database changes and the namespace is invalidated while the
	// old value is being loaded; that value must not be served later.
	Fetch(ns, "n", time.Minute, func() (string, error) {
		ns.Invalidate()
		return "stale", nil
	})
	if v, _ := Fetch(ns, "n", time.Minute, func() (string, error) { return "fresh", nil }); v != "fresh" {
		t.Errorf("got %q, want fresh", v)
	}
}

func TestFetchLoadErrorNotCached(t *testing.T) {
	c, _ := newBolt(t)
	ns := NS(c, "x")
	if _, err := Fetch(ns, "k", time.Minute, func() (int, error) { return 0, fmt.Errorf("db down") }); err == nil {
		t.Fatal("want error")
	}
	if v, _ := Fetch(ns, "k", time.Minute, func() (int, error) { return 7, nil }); v != 7 {
		t.Errorf("got %d, want 7", v)
	}
}
