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
	// GetMany returns the values for keys in order, nil for a miss.
	GetMany(keys []string) ([][]byte, error)
	// Set stores val for ttl. A ttl <= 0 never expires.
	Set(key string, val []byte, ttl time.Duration) error
	// SetMany stores all entries for ttl.
	SetMany(entries []Entry, ttl time.Duration) error
	Delete(keys ...string) error
	Close() error
}

type Entry struct {
	Key string
	Val []byte
}

// Nop caches nothing. It's the default when no cache is configured.
type Nop struct{}

func (Nop) Get(string) ([]byte, bool, error)        { return nil, false, nil }
func (Nop) GetMany(keys []string) ([][]byte, error) { return make([][]byte, len(keys)), nil }
func (Nop) Set(string, []byte, time.Duration) error { return nil }
func (Nop) SetMany([]Entry, time.Duration) error    { return nil }
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

// newGen stores a fresh generation.
func (n Namespace) newGen() (string, error) {
	gen := nextGen()
	return gen, n.c.Set(n.genKey(), []byte(gen), 0)
}

// nextGen returns a new generation. Generations are timestamps rather than
// counters so a lost generation key (evicted, or a restart) can't make an
// old generation, and its stale keys, current again.
func nextGen() string {
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
	return strconv.FormatInt(g, 36)
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
	return fetch(n.c, full, ttl, load)
}

// FetchKey is Fetch for a key outside any namespace. It suits mappings
// that don't change, like a username to a user ID; remove one with Delete.
func FetchKey[T any](c Cache, key string, ttl time.Duration, load func() (T, error)) (T, error) {
	return fetch(c, key, ttl, load)
}

func fetch[T any](c Cache, full string, ttl time.Duration, load func() (T, error)) (T, error) {
	if b, ok, err := c.Get(full); err != nil {
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
	} else if err := c.Set(full, b, ttl); err != nil {
		slog.Warn("cache", "op", "set", "key", full, "err", err)
	}
	return v, nil
}

// FetchMany is Fetch for key in the namespaces kind:{id}, one per id, with
// one batched cache read and write, and one call to load for all misses.
// load gets the missing IDs and returns the values it found. IDs it leaves
// out are left out of the result and not cached, so it must return an
// entry (if need be a zero value) for every ID that exists.
func FetchMany[K comparable, T any](c Cache, kind string, ids []K, key string, ttl time.Duration, load func(missing []K) (map[K]T, error)) (map[K]T, error) {
	ids = unique(ids)
	out := make(map[K]T, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	full, err := manyKeys(c, kind, ids, key)
	if err != nil {
		slog.Warn("cache", "op", "keys", "kind", kind, "err", err)
		return load(ids)
	}
	vals, err := c.GetMany(full)
	if err != nil {
		slog.Warn("cache", "op", "get", "kind", kind, "err", err)
		vals = make([][]byte, len(ids))
	}
	var missing []K
	keyOf := map[K]string{}
	for i, id := range ids {
		if vals[i] != nil {
			var v T
			if err := json.Unmarshal(vals[i], &v); err == nil {
				out[id] = v
				continue
			}
		}
		missing = append(missing, id)
		keyOf[id] = full[i]
	}
	if len(missing) == 0 {
		return out, nil
	}
	loaded, err := load(missing)
	if err != nil {
		return nil, err
	}
	var entries []Entry
	for id, v := range loaded {
		k, ok := keyOf[id]
		if !ok {
			continue // not asked for
		}
		out[id] = v
		if b, err := json.Marshal(v); err != nil {
			slog.Warn("cache", "op", "encode", "key", k, "err", err)
		} else {
			entries = append(entries, Entry{k, b})
		}
	}
	if err := c.SetMany(entries, ttl); err != nil {
		slog.Warn("cache", "op", "set", "kind", kind, "err", err)
	}
	return out, nil
}

// manyKeys is Namespace.key for key in kind:{id} for each id, resolving
// (and where missing, creating) the generations in one read and one write.
func manyKeys[K comparable](c Cache, kind string, ids []K, key string) ([]string, error) {
	names := make([]string, len(ids))
	genKeys := make([]string, len(ids))
	for i, id := range ids {
		names[i] = NS(c, kind, id).name
		genKeys[i] = "gen:" + names[i]
	}
	gens, err := c.GetMany(genKeys)
	if err != nil {
		return nil, err
	}
	var fresh []Entry
	for i := range gens {
		if gens[i] == nil {
			gens[i] = []byte(nextGen())
			fresh = append(fresh, Entry{genKeys[i], gens[i]})
		}
	}
	if len(fresh) > 0 {
		if err := c.SetMany(fresh, 0); err != nil {
			return nil, err
		}
	}
	full := make([]string, len(ids))
	for i := range ids {
		full[i] = names[i] + "@" + string(gens[i]) + ":" + key
	}
	return full, nil
}

// unique returns ids without duplicates, in first-seen order.
func unique[K comparable](ids []K) []K {
	seen := make(map[K]bool, len(ids))
	out := make([]K, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}
