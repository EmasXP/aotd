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

func TestBoltGetSetMany(t *testing.T) {
	c, _ := newBolt(t)
	now := time.Now()
	c.now = func() time.Time { return now }
	c.SetMany([]Entry{{"a", []byte("1")}, {"b", []byte("2")}}, time.Minute)
	c.Set("c", []byte("3"), 0)
	vals, err := c.GetMany([]string{"a", "missing", "c", "b"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"1", "", "3", "2"}
	for i, v := range vals {
		if (v == nil) != (want[i] == "") || string(v) != want[i] {
			t.Errorf("vals[%d] = %q, want %q", i, v, want[i])
		}
	}
	now = now.Add(2 * time.Minute)
	if vals, _ := c.GetMany([]string{"a", "c"}); vals[0] != nil || string(vals[1]) != "3" {
		t.Errorf("after expiry: %q", vals)
	}
}

func TestFetchMany(t *testing.T) {
	c, _ := newBolt(t)
	var calls [][]int
	load := func(missing []int) (map[int]string, error) {
		calls = append(calls, missing)
		out := map[int]string{}
		for _, id := range missing {
			if id != 404 { // 404 doesn't exist
				out[id] = fmt.Sprintf("v%d-%d", id, len(calls))
			}
		}
		return out, nil
	}
	got, _ := FetchMany(c, "item", []int{1, 2, 404, 2}, "row", time.Minute, load)
	if len(calls) != 1 || len(calls[0]) != 3 || len(got) != 2 || got[1] != "v1-1" {
		t.Fatalf("first: got %v, calls %v", got, calls)
	}
	// Fetch shares the entries.
	if v, _ := Fetch(NS(c, "item", 1), "row", time.Minute, func() (string, error) { return "fetched", nil }); v != "v1-1" {
		t.Errorf("Fetch = %q", v)
	}
	// Only misses are loaded; the missing ID wasn't cached and is asked again.
	NS(c, "item", 2).Invalidate()
	got, _ = FetchMany(c, "item", []int{1, 2, 3, 404}, "row", time.Minute, load)
	if len(calls) != 2 || fmt.Sprint(calls[1]) != "[2 3 404]" {
		t.Errorf("second load got %v", calls[1:])
	}
	if got[1] != "v1-1" || got[2] != "v2-2" || got[3] != "v3-2" {
		t.Errorf("second: %v", got)
	}
	// All hits: no load.
	FetchMany(c, "item", []int{1, 2, 3}, "row", time.Minute, load)
	if len(calls) != 2 {
		t.Errorf("load called on all hits: %v", calls[2:])
	}
}

func TestFetchManyDuringInvalidate(t *testing.T) {
	c, _ := newBolt(t)
	FetchMany(c, "item", []int{1}, "row", time.Minute, func([]int) (map[int]string, error) {
		NS(c, "item", 1).Invalidate()
		return map[int]string{1: "stale"}, nil
	})
	got, _ := FetchMany(c, "item", []int{1}, "row", time.Minute, func([]int) (map[int]string, error) {
		return map[int]string{1: "fresh"}, nil
	})
	if got[1] != "fresh" {
		t.Errorf("got %q, want fresh", got[1])
	}
}

func TestFetchManyLoadError(t *testing.T) {
	c, _ := newBolt(t)
	if _, err := FetchMany(c, "item", []int{1}, "row", time.Minute, func([]int) (map[int]string, error) {
		return nil, fmt.Errorf("db down")
	}); err == nil {
		t.Fatal("want error")
	}
	got, _ := FetchMany(c, "item", []int{1}, "row", time.Minute, func([]int) (map[int]string, error) {
		return map[int]string{1: "ok"}, nil
	})
	if got[1] != "ok" {
		t.Errorf("got %q", got[1])
	}
}
