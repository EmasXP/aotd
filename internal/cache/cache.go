// Package cache is a key-value cache behind a small interface, so the
// backend (bbolt today) can be swapped for something like Valkey later.
//
// Invalidation is done with namespaces rather than key scans: every
// namespace has a generation that is part of its keys, and Invalidate
// replaces the generation so all old keys become unreachable at once. The
// orphans are left for their TTL to remove. This needs nothing but
// Get/Set/Delete from the backend.
package cache

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type Cache interface {
	// Get returns the value for key. Expired keys are misses.
	Get(key string) (val []byte, ok bool, err error)
	// Set stores val for ttl. A ttl <= 0 never expires.
	Set(key string, val []byte, ttl time.Duration) error
	Delete(keys ...string) error
	Close() error
}

// Nop caches nothing. It's the default when no cache is configured.
type Nop struct{}

func (Nop) Get(string) ([]byte, bool, error)        { return nil, false, nil }
func (Nop) Set(string, []byte, time.Duration) error { return nil }
func (Nop) Delete(...string) error                  { return nil }
func (Nop) Close() error                            { return nil }

// Namespace groups keys that are invalidated together, e.g. everything
// cached about one user.
type Namespace struct {
	c    Cache
	name string
}

// NS returns the namespace named by parts joined with ":", e.g.
// NS(c, "user", 42) is "user:42".
func NS(c Cache, parts ...any) Namespace {
	s := make([]string, len(parts))
	for i, p := range parts {
		s[i] = fmt.Sprint(p)
	}
	return Namespace{c: c, name: strings.Join(s, ":")}
}

func (n Namespace) genKey() string { return "gen:" + n.name }

// Invalidate makes every key in the namespace a miss. Call it after the
// database change has committed.
func (n Namespace) Invalidate() error {
	_, err := n.newGen()
	return err
}

// key resolves the generation and returns the full key for k.
func (n Namespace) key(k string) (string, error) {
	g, ok, err := n.c.Get(n.genKey())
	if err != nil {
		return "", err
	}
	gen := string(g)
	if !ok {
		if gen, err = n.newGen(); err != nil {
			return "", err
		}
	}
	return n.name + "@" + gen + ":" + k, nil
}

// lastGen keeps generations from this process strictly increasing even if
// the clock is coarse or steps back.
var lastGen atomic.Int64

// newGen stores a fresh generation. Generations are timestamps rather than
// counters so a lost generation key (evicted, or a restart) can't make an
// old generation, and its stale keys, current again.
func (n Namespace) newGen() (string, error) {
	g := time.Now().UnixNano()
	for {
		last := lastGen.Load()
		if g <= last {
			g = last + 1
		}
		if lastGen.CompareAndSwap(last, g) {
			break
		}
	}
	gen := strconv.FormatInt(g, 36)
	return gen, n.c.Set(n.genKey(), []byte(gen), 0)
}

// Fetch returns the cached value for key in n, or calls load and caches its
// result as JSON for ttl. Cache errors are logged and fall back to load, so
// a broken cache never breaks a page.
//
// The generation is resolved once, so a value loaded while n is being
// invalidated is stored under the old generation and never served.
func Fetch[T any](n Namespace, key string, ttl time.Duration, load func() (T, error)) (T, error) {
	full, err := n.key(key)
	if err != nil {
		slog.Warn("cache", "op", "key", "ns", n.name, "err", err)
		return load()
	}
	if b, ok, err := n.c.Get(full); err != nil {
		slog.Warn("cache", "op", "get", "key", full, "err", err)
	} else if ok {
		var v T
		if err := json.Unmarshal(b, &v); err == nil {
			return v, nil
		}
	}
	v, err := load()
	if err != nil {
		return v, err
	}
	if b, err := json.Marshal(v); err != nil {
		slog.Warn("cache", "op", "encode", "key", full, "err", err)
	} else if err := n.c.Set(full, b, ttl); err != nil {
		slog.Warn("cache", "op", "set", "key", full, "err", err)
	}
	return v, nil
}
