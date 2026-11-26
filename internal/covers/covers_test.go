package covers

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

const testMBID = "b1392450-e666-3926-a536-22c65f834433"

func testJPEG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newTestCache(t *testing.T, h http.HandlerFunc) (*Cache, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	c := New(t.TempDir(), "AOTD/0.1 ( test@example.com )")
	c.BaseURL = srv.URL
	c.limiter = rate.NewLimiter(rate.Inf, 1)
	return c, &hits
}

func TestFetchCachesOnDisk(t *testing.T) {
	jpg := testJPEG(t)
	c, hits := newTestCache(t, func(w http.ResponseWriter, r *http.Request) {
		if ua := r.Header.Get("User-Agent"); ua != "AOTD/0.1 ( test@example.com )" {
			t.Errorf("User-Agent = %q", ua)
		}
		if r.URL.Path != "/release-group/"+testMBID+"/front-500" {
			http.NotFound(w, r)
			return
		}
		w.Write(jpg)
	})

	p1, err := c.Fetch(context.Background(), testMBID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p1)
	if err != nil || !bytes.Equal(got, jpg) {
		t.Fatalf("cached file mismatch: %v", err)
	}
	// Second call hits disk only.
	p2, err := c.Fetch(context.Background(), testMBID)
	if err != nil || p2 != p1 {
		t.Fatalf("second Fetch = %q, %v", p2, err)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("hits = %d, want 1", n)
	}
	// Invalid MBID never hits network.
	if _, err := c.Fetch(context.Background(), "../../etc/passwd"); !errors.Is(err, ErrNotFound) {
		t.Errorf("invalid mbid err = %v", err)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("hits after invalid mbid = %d, want 1", n)
	}
}

func TestMissGraduatedBackoffAndRecovery(t *testing.T) {
	jpg := testJPEG(t)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	var available atomic.Bool

	c, hits := newTestCache(t, func(w http.ResponseWriter, r *http.Request) {
		if !available.Load() {
			http.NotFound(w, r)
			return
		}
		w.Write(jpg)
	})
	c.now = func() time.Time { return now }

	// Attempt 1 -> 1h cooldown, Attempt 2 -> 6h cooldown.
	for i, wait := range []time.Duration{time.Hour, 6 * time.Hour} {
		if _, err := c.Fetch(context.Background(), testMBID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("attempt %d err = %v, want ErrNotFound", i+1, err)
		}
		if n := hits.Load(); n != int32(i+1) {
			t.Fatalf("attempt %d hits = %d, want %d", i+1, n, i+1)
		}
		// Inside cooldown: no network call.
		now = now.Add(wait - time.Minute)
		if _, err := c.Fetch(context.Background(), testMBID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("attempt %d during cooldown err = %v", i+1, err)
		}
		if n := hits.Load(); n != int32(i+1) {
			t.Fatalf("attempt %d hit network during cooldown (%d)", i+1, n)
		}
		now = now.Add(2 * time.Minute)
	}

	// Cover is now uploaded on MusicBrainz -> next retry succeeds and clears .404.
	available.Store(true)
	p, err := c.Fetch(context.Background(), testMBID)
	if err != nil {
		t.Fatalf("recovery Fetch err = %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("cover file missing: %v", err)
	}
	if _, err := os.Stat(c.missPath(testMBID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".404 marker still present after recovery")
	}
}

func TestSingleflightConcurrentFetch(t *testing.T) {
	jpg := testJPEG(t)
	started := make(chan struct{})
	release := make(chan struct{})
	c, hits := newTestCache(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.Write(jpg)
	})

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.Fetch(context.Background(), testMBID)
		}(i)
		if i == 0 {
			<-started
		}
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d err = %v", i, err)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("hits = %d, want 1", n)
	}
}

func TestThrottleDoesNotRecord404(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	c, hits := newTestCache(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "10")
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	c.now = func() time.Time { return now }

	if _, err := c.Fetch(context.Background(), testMBID); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("503 err = %v, want non-NotFound error", err)
	}
	if _, err := os.Stat(c.missPath(testMBID)); !errors.Is(err, os.ErrNotExist) {
		t.Error("503 should not create a .404 marker")
	}
	// Fails fast during 10s Retry-After.
	now = now.Add(5 * time.Second)
	if _, err := c.Fetch(context.Background(), testMBID); err == nil {
		t.Fatal("expected backoff error")
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("hits = %d, want 1", n)
	}
}
