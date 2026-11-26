// Package covers caches Cover Art Archive images on disk so posted albums
// aren't hotlinked on every page load. Missing covers (404s) are remembered
// with graduated backoff so newly uploaded art on MusicBrainz still shows up
// soon without hammering CAA on every view.
package covers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/EmasXP/aotd/internal/musicbrainz"
)

const (
	DefaultBaseURL = "https://coverartarchive.org"
	MaxBytes       = 5 << 20
	throttleWait   = 15 * time.Second
)

var (
	ErrNotFound = errors.New("covers: not found")

	// Graduated cooldown for missing covers: 1h, 6h, 24h, 7d, then 30d.
	// Short at first so someone who notices a missing cover and uploads it
	// to MusicBrainz sees it appear quickly; longer once it stays missing.
	missBackoff = []time.Duration{
		time.Hour,
		6 * time.Hour,
		24 * time.Hour,
		7 * 24 * time.Hour,
		30 * 24 * time.Hour,
	}

	allowedTypes = map[string]bool{
		"image/jpeg": true,
		"image/png":  true,
		"image/webp": true,
		"image/gif":  true,
	}
)

type Cache struct {
	Dir       string
	BaseURL   string
	UserAgent string
	HTTP      *http.Client
	Log       *slog.Logger

	limiter      *rate.Limiter
	now          func() time.Time
	mu           sync.Mutex
	inflight     map[string]*inflightCall
	blockedUntil time.Time
}

type inflightCall struct {
	done    chan struct{}
	path    string
	err     error
	waiters int
	cancel  context.CancelFunc
}

func New(dir, userAgent string) *Cache {
	return &Cache{
		Dir:       dir,
		BaseURL:   DefaultBaseURL,
		UserAgent: userAgent,
		HTTP:      &http.Client{Timeout: 15 * time.Second},
		Log:       slog.Default(),
		limiter:   rate.NewLimiter(rate.Every(500*time.Millisecond), 2),
		now:       time.Now,
		inflight:  map[string]*inflightCall{},
	}
}

func (c *Cache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// Fetch returns the local file path of the cached 500px front cover for a
// release-group MBID, downloading it from Cover Art Archive on a cache miss.
func (c *Cache) Fetch(ctx context.Context, mbid string) (string, error) {
	if !musicbrainz.ValidMBID(mbid) {
		return "", ErrNotFound
	}
	imgPath := filepath.Join(c.Dir, mbid)
	if _, err := os.Stat(imgPath); err == nil {
		return imgPath, nil
	}
	if c.inMissCooldown(mbid) {
		return "", ErrNotFound
	}
	return c.do(ctx, mbid, func(ctx context.Context) (string, error) {
		// Re-check disk in case another fetch just finished.
		if _, err := os.Stat(imgPath); err == nil {
			return imgPath, nil
		}
		if c.inMissCooldown(mbid) {
			return "", ErrNotFound
		}
		return c.download(ctx, mbid, imgPath)
	})
}

func (c *Cache) do(ctx context.Context, key string, fn func(context.Context) (string, error)) (string, error) {
	c.mu.Lock()
	if c.inflight == nil {
		c.inflight = map[string]*inflightCall{}
	}
	call, joined := c.inflight[key]
	if joined {
		call.waiters++
		c.mu.Unlock()
	} else {
		callCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		call = &inflightCall{done: make(chan struct{}), waiters: 1, cancel: cancel}
		c.inflight[key] = call
		c.mu.Unlock()
		go func() {
			defer cancel()
			p, err := fn(callCtx)
			c.mu.Lock()
			call.path, call.err = p, err
			if c.inflight[key] == call {
				delete(c.inflight, key)
			}
			c.mu.Unlock()
			close(call.done)
		}()
	}

	select {
	case <-call.done:
		return call.path, call.err
	case <-ctx.Done():
		c.mu.Lock()
		call.waiters--
		if call.waiters == 0 {
			call.cancel()
			if c.inflight[key] == call {
				delete(c.inflight, key)
			}
		}
		c.mu.Unlock()
		return "", ctx.Err()
	}
}

func (c *Cache) download(ctx context.Context, mbid, imgPath string) (string, error) {
	if err := c.checkBackoff(); err != nil {
		return "", err
	}
	if err := c.limiter.Wait(ctx); err != nil {
		return "", err
	}
	if err := c.checkBackoff(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/release-group/"+mbid+"/front-500", nil)
	if err != nil {
		return "", err
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	switch res.StatusCode {
	case http.StatusNotFound, http.StatusBadRequest:
		c.recordMiss(mbid)
		return "", ErrNotFound
	case http.StatusServiceUnavailable, http.StatusTooManyRequests:
		c.recordThrottle(res.Header.Get("Retry-After"))
		c.Log.Warn("coverartarchive is throttling us", "status", res.StatusCode, "mbid", mbid)
		return "", fmt.Errorf("covers: %s", res.Status)
	case http.StatusOK:
	default:
		return "", fmt.Errorf("covers: %s", res.Status)
	}

	data, err := io.ReadAll(io.LimitReader(res.Body, MaxBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) == 0 || len(data) > MaxBytes {
		return "", fmt.Errorf("covers: invalid image size %d", len(data))
	}
	if ct := http.DetectContentType(data); !allowedTypes[ct] {
		return "", fmt.Errorf("covers: unexpected content type %q", ct)
	}
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(c.Dir, mbid+".tmp-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	if err := os.Rename(tmpName, imgPath); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	os.Remove(c.missPath(mbid))
	return imgPath, nil
}

func (c *Cache) missPath(mbid string) string { return filepath.Join(c.Dir, mbid+".404") }

func (c *Cache) inMissCooldown(mbid string) bool {
	p := c.missPath(mbid)
	info, err := os.Stat(p)
	if err != nil {
		return false
	}
	b, _ := os.ReadFile(p)
	attempts, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if attempts < 1 {
		attempts = 1
	}
	idx := min(attempts-1, len(missBackoff)-1)
	return c.clock().Before(info.ModTime().Add(missBackoff[idx]))
}

func (c *Cache) recordMiss(mbid string) {
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		return
	}
	p := c.missPath(mbid)
	attempts := 0
	if b, err := os.ReadFile(p); err == nil {
		attempts, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	attempts++
	_ = os.WriteFile(p, []byte(strconv.Itoa(attempts)), 0o644)
	now := c.clock()
	_ = os.Chtimes(p, now, now)
}

func (c *Cache) checkBackoff() error {
	c.mu.Lock()
	until := c.blockedUntil
	c.mu.Unlock()
	if c.clock().Before(until) {
		return errors.New("covers: backing off after throttle")
	}
	return nil
}

func (c *Cache) recordThrottle(retryAfter string) {
	wait := throttleWait
	if secs, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && secs > 0 {
		wait = min(time.Duration(secs)*time.Second, 5*time.Minute)
	}
	now := c.clock()
	c.mu.Lock()
	if until := now.Add(wait); until.After(c.blockedUntil) {
		c.blockedUntil = until
	}
	c.mu.Unlock()
}
