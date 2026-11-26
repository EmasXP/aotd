package musicbrainz

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

const searchJSON = `{"release-groups":[{"id":"b1392450-e666-3926-a536-22c65f834433","title":"OK Computer",
"first-release-date":"1997-05-21","primary-type":"Album",
"artist-credit":[{"name":"Radiohead","joinphrase":""}]},
{"id":"11111111-2222-3333-4444-555555555555","title":"Duets","first-release-date":"",
"primary-type":"Album","secondary-types":["Live"],
"artist-credit":[{"name":"A","joinphrase":" & "},{"name":"B","joinphrase":""}]}]}`

func newTestClient(t *testing.T, h http.HandlerFunc) (*Client, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	c := New("test@example.com")
	c.BaseURL = srv.URL
	c.limiter = rate.NewLimiter(rate.Inf, 1)
	return c, &hits
}

func TestSearch(t *testing.T) {
	c, hits := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if ua := r.Header.Get("User-Agent"); ua != "AOTD/0.1 ( test@example.com )" {
			t.Errorf("User-Agent = %q", ua)
		}
		if q := r.URL.Query().Get("query"); q != `releasegroup:(ok computer\!) AND artist:(radiohead) AND primarytype:album` {
			t.Errorf("query = %q", q)
		}
		w.Write([]byte(searchJSON))
	})
	albums, err := c.Search(context.Background(), "OK Computer!", "Radiohead", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(albums) != 2 {
		t.Fatalf("got %d albums", len(albums))
	}
	if a := albums[0]; a.Title != "OK Computer" || a.Artist != "Radiohead" || a.Year != 1997 || a.Type != "Album" {
		t.Errorf("album[0] = %+v", a)
	}
	if a := albums[1]; a.Artist != "A & B" || a.Year != 0 || a.Type != "Album · Live" {
		t.Errorf("album[1] = %+v", a)
	}
	// Second identical search is served from cache.
	if _, err := c.Search(context.Background(), "ok computer!", " radiohead", 10); err != nil {
		t.Fatal(err)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("server hit %d times, want 1 (cache)", n)
	}
}

func TestBuildQueryNeutralisesOperators(t *testing.T) {
	tests := []struct{ album, artist, want string }{
		{`back in "black" OR x`, `AC/DC`, `releasegroup:(back in \"black\" or x) AND artist:(ac\/dc) AND primarytype:album`},
		{`Blue`, ``, `releasegroup:(blue) AND primarytype:album`},
		{``, `Björk`, `artist:(björk) AND primarytype:album`},
	}
	for _, tt := range tests {
		if got := buildQuery(tt.album, tt.artist); got != tt.want {
			t.Errorf("buildQuery(%q, %q) = %q\nwant %q", tt.album, tt.artist, got, tt.want)
		}
	}
}

func TestLookup(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/release-group/b1392450-e666-3926-a536-22c65f834433") {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"id":"b1392450-e666-3926-a536-22c65f834433","title":"OK Computer","first-release-date":"1997","primary-type":"Album","artist-credit":[{"name":"Radiohead"}]}`))
	})
	a, err := c.Lookup(context.Background(), "b1392450-e666-3926-a536-22c65f834433")
	if err != nil || a.Title != "OK Computer" || a.Year != 1997 {
		t.Fatalf("Lookup = %+v, %v", a, err)
	}
	if _, err := c.Lookup(context.Background(), "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"); err != ErrNotFound {
		t.Errorf("missing mbid err = %v", err)
	}
	if _, err := c.Lookup(context.Background(), "../../etc"); err != ErrNotFound {
		t.Errorf("invalid mbid err = %v", err)
	}
}

func TestRateLimit(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) })
	c.limiter = rate.NewLimiter(rate.Every(200*time.Millisecond), 1)
	start := time.Now()
	for _, q := range []string{"a", "b", "c"} {
		if _, err := c.Search(context.Background(), q, "", 5); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < 350*time.Millisecond {
		t.Errorf("3 requests took %v; limiter not applied", d)
	}
}

func TestLookupURL(t *testing.T) {
	const spotify = "https://open.spotify.com/album/0tzfI6NFJqcJkWb23R3lRZ"
	c, hits := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/url" && r.URL.Query().Get("resource") == spotify:
			w.Write([]byte(`{"resource":"` + spotify + `","relations":[{"type":"free streaming","release":{"id":"d7881c68-914a-4da3-8a7d-be89f0201307"}}]}`))
		case r.URL.Path == "/release/d7881c68-914a-4da3-8a7d-be89f0201307" && strings.Contains(r.URL.RawQuery, "release-groups"):
			w.Write([]byte(`{"id":"d7881c68-914a-4da3-8a7d-be89f0201307","release-group":{"id":"b1392450-e666-3926-a536-22c65f834433","title":"OK Computer","first-release-date":"1997-05-21","primary-type":"Album","artist-credit":[{"name":"Radiohead"}]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"Not Found"}`))
		}
	})
	a, err := c.LookupURL(context.Background(), spotify)
	if err != nil || a.MBID != "b1392450-e666-3926-a536-22c65f834433" || a.Artist != "Radiohead" || a.Year != 1997 {
		t.Fatalf("LookupURL = %+v, %v", a, err)
	}
	if _, err := c.LookupURL(context.Background(), spotify); err != nil || hits.Load() != 2 {
		t.Errorf("cached lookup: %v, %d requests", err, hits.Load())
	}
	// The lookup on submit can use the cache too.
	if _, err := c.Lookup(context.Background(), a.MBID); err != nil || hits.Load() != 2 {
		t.Errorf("lookup after URL lookup: %v, %d requests", err, hits.Load())
	}
	for range 2 {
		if _, err := c.LookupURL(context.Background(), "https://open.spotify.com/album/unknown"); err != ErrNotFound {
			t.Errorf("unknown URL err = %v", err)
		}
	}
	if hits.Load() != 3 {
		t.Errorf("%d requests; unknown URL not cached", hits.Load())
	}
}

func TestReleaseGroupReleasesPages(t *testing.T) {
	var offsets []string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/release" || r.URL.Query().Get("release-group") != "b1392450-e666-3926-a536-22c65f834433" {
			http.NotFound(w, r)
			return
		}
		off := r.URL.Query().Get("offset")
		offsets = append(offsets, off)
		w.Write([]byte(`{"release-count":150,"releases":[{"id":"r` + off + `","status":"Official","date":"1997",
"media":[{"track-count":12},{"track-count":3}],
"relations":[{"url":{"resource":"https://example.com/` + off + `"}},{"type":"other","artist":{}}]},{}]}`))
	})
	rels, err := c.ReleaseGroupReleases(context.Background(), "b1392450-e666-3926-a536-22c65f834433")
	if err != nil {
		t.Fatal(err)
	}
	var urls []string
	for _, r := range rels {
		urls = append(urls, r.URLs...)
	}
	if strings.Join(offsets, ",") != "0,100" || strings.Join(urls, " ") != "https://example.com/0 https://example.com/100" {
		t.Errorf("offsets %v, urls %v", offsets, urls)
	}
	if len(rels) != 4 || rels[0].MBID != "r0" || rels[0].Status != "Official" || rels[0].Date != "1997" || rels[0].Tracks != 15 {
		t.Errorf("releases = %+v", rels)
	}
}

func TestReleaseLength(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/release/11111111-2222-3333-4444-555555555555":
			w.Write([]byte(`{"media":[{"track-count":2,"tracks":[{"length":60000},{"length":1500}]},{"track-count":1,"tracks":[{"length":30000}]}]}`))
		case "/release/22222222-2222-3333-4444-555555555555":
			w.Write([]byte(`{"media":[{"track-count":2,"tracks":[{"length":60000},{"length":null}]}]}`))
		default:
			http.NotFound(w, r)
		}
	})
	n, d, err := c.ReleaseLength(context.Background(), "11111111-2222-3333-4444-555555555555")
	if err != nil || n != 3 || d != 91500*time.Millisecond {
		t.Errorf("ReleaseLength = %d, %v, %v", n, d, err)
	}
	n, d, err = c.ReleaseLength(context.Background(), "22222222-2222-3333-4444-555555555555")
	if err != nil || n != 2 || d != 0 {
		t.Errorf("with an unknown track length = %d, %v, %v; want 2 tracks, length 0", n, d, err)
	}
}

func TestReleaseGroupMetaAndArtist(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/release-group/b1392450-e666-3926-a536-22c65f834433":
			if inc := r.URL.Query().Get("inc"); inc != "artist-credits genres" {
				t.Errorf("inc = %q", inc)
			}
			w.Write([]byte(`{"artist-credit":[{"name":"Radiohead","artist":{"id":"a74b1b7f-71a5-4011-9441-d0b5e4122711","name":"Radiohead"}},{"name":"?","artist":{"id":"bad"}}],
"genres":[{"name":"alternative rock","count":27},{"name":"art rock","count":15}]}`))
		case "/artist/a74b1b7f-71a5-4011-9441-d0b5e4122711":
			w.Write([]byte(`{"id":"a74b1b7f-71a5-4011-9441-d0b5e4122711","name":"Radiohead","country":"GB","area":{"name":"United Kingdom"},"genres":[{"name":"art rock","count":9}]}`))
		default:
			http.NotFound(w, r)
		}
	})
	m, err := c.ReleaseGroupMeta(context.Background(), "b1392450-e666-3926-a536-22c65f834433")
	if err != nil || len(m.Credits) != 1 || m.Credits[0].Name != "Radiohead" || len(m.Genres) != 2 || m.Genres[0] != (Genre{"alternative rock", 27}) {
		t.Errorf("ReleaseGroupMeta = %+v, %v", m, err)
	}
	a, err := c.LookupArtist(context.Background(), m.Credits[0].MBID)
	if err != nil || a.Country != "GB" || a.Area != "United Kingdom" || len(a.Genres) != 1 {
		t.Errorf("LookupArtist = %+v, %v", a, err)
	}
}

func TestThrottleIsLogged(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("Your requests are exceeding the allowable rate limit."))
	})
	var logs bytes.Buffer
	c.Log = slog.New(slog.NewTextHandler(&logs, nil))
	_, err := c.Lookup(context.Background(), "b1392450-e666-3926-a536-22c65f834433")
	var se *StatusError
	if !errors.As(err, &se) || se.RetryAfter != "5" || !strings.Contains(se.Body, "rate limit") {
		t.Fatalf("err = %#v", err)
	}
	if out := logs.String(); !strings.Contains(out, "throttling") || !strings.Contains(out, "rate limit") {
		t.Errorf("log = %q", out)
	}
}

func TestParseURL(t *testing.T) {
	const id = "b1392450-e666-3926-a536-22c65f834433"
	tests := []struct {
		in   string
		want Ref
	}{
		{"https://musicbrainz.org/release-group/" + id, Ref{MBID: id}},
		{" musicbrainz.org/release-group/" + strings.ToUpper(id) + "/ ", Ref{MBID: id}},
		{"https://beta.musicbrainz.org/release/" + id + "/cover-art?x=1", Ref{Release: true, MBID: id}},
		{"http://www.musicbrainz.org/release/" + id, Ref{Release: true, MBID: id}},
		{"https://musicbrainz.org/artist/" + id, Ref{}},
		{"https://musicbrainz.org/release-group/not-an-mbid", Ref{}},
		{"https://evil.example/release-group/" + id, Ref{}},
		{"https://open.spotify.com/album/6dVIqQ8qmQ5GBnJ9shOYGE", Ref{}},
		{"OK Computer", Ref{}},
	}
	for _, tt := range tests {
		got, ok := ParseURL(tt.in)
		if ok != (tt.want != Ref{}) || got != tt.want {
			t.Errorf("ParseURL(%q) = %+v, %v; want %+v", tt.in, got, ok, tt.want)
		}
	}
}

func TestLookupRelease(t *testing.T) {
	c, hits := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/release/30702389-5c67-4438-9ea0-2351c8de0f1d" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"release-group":{"id":"b1392450-e666-3926-a536-22c65f834433","title":"OK Computer","primary-type":"Album","artist-credit":[{"name":"Radiohead"}]}}`))
	})
	ctx := context.Background()
	a, err := c.LookupRef(ctx, Ref{Release: true, MBID: "30702389-5c67-4438-9ea0-2351c8de0f1d"})
	if err != nil || a.MBID != "b1392450-e666-3926-a536-22c65f834433" || a.Title != "OK Computer" {
		t.Fatalf("LookupRef = %+v, %v", a, err)
	}
	// Both the release and its group are now cached.
	c.LookupRelease(ctx, "30702389-5c67-4438-9ea0-2351c8de0f1d")
	c.Lookup(ctx, a.MBID)
	if n := hits.Load(); n != 1 {
		t.Errorf("hits = %d, want 1", n)
	}
}

func TestBackoffRetryAfterAndReset(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	var status atomic.Int32
	status.Store(http.StatusServiceUnavailable)
	var retryAfter atomic.Value
	retryAfter.Store("15")

	c, hits := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if ra := retryAfter.Load().(string); ra != "" {
			w.Header().Set("Retry-After", ra)
		}
		if code := int(status.Load()); code != http.StatusOK {
			w.WriteHeader(code)
			return
		}
		w.Write([]byte(`{"id":"b1392450-e666-3926-a536-22c65f834433","title":"OK Computer"}`))
	})
	c.now = func() time.Time { return now }
	const id = "b1392450-e666-3926-a536-22c65f834433"

	// First call gets 503 with Retry-After: 15.
	var se *StatusError
	if _, err := c.Lookup(context.Background(), id); !errors.As(err, &se) {
		t.Fatalf("first err = %v, want StatusError", err)
	}
	// Calls during the 15s window fail fast without hitting the server.
	now = now.Add(14 * time.Second)
	if _, err := c.Lookup(context.Background(), id); !errors.Is(err, ErrBackoff) {
		t.Fatalf("during backoff err = %v, want ErrBackoff", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1 (should fail fast)", hits.Load())
	}

	// Once the window passes, a 200 OK succeeds and clears backoff state.
	now = now.Add(2 * time.Second)
	status.Store(http.StatusOK)
	retryAfter.Store("")
	if a, err := c.Lookup(context.Background(), id); err != nil || a.Title != "OK Computer" {
		t.Fatalf("after backoff: %+v, %v", a, err)
	}
	if hits.Load() != 2 {
		t.Fatalf("hits = %d, want 2", hits.Load())
	}
}

func TestBackoffExponentialAndForbidden(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	var status atomic.Int32
	status.Store(http.StatusTooManyRequests)

	c, hits := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
	})
	c.now = func() time.Time { return now }
	const id = "b1392450-e666-3926-a536-22c65f834433"

	// Strike 1: 5s, Strike 2: 10s, Strike 3: 20s.
	for i, want := range []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second} {
		_, err := c.Lookup(context.Background(), id)
		var se *StatusError
		if !errors.As(err, &se) {
			t.Fatalf("strike %d err = %v, want StatusError", i+1, err)
		}
		now = now.Add(want - time.Second)
		if _, err := c.Lookup(context.Background(), id); !errors.Is(err, ErrBackoff) {
			t.Fatalf("strike %d before %v: err = %v, want ErrBackoff", i+1, want, err)
		}
		now = now.Add(2 * time.Second)
	}
	if hits.Load() != 3 {
		t.Fatalf("hits = %d, want 3", hits.Load())
	}

	// 403 Forbidden jumps straight to forbiddenBackoff (5m).
	status.Store(http.StatusForbidden)
	_, _ = c.Lookup(context.Background(), id)
	now = now.Add(forbiddenBackoff - time.Second)
	if _, err := c.Lookup(context.Background(), id); !errors.Is(err, ErrBackoff) {
		t.Fatalf("403 backoff err = %v, want ErrBackoff", err)
	}
	if hits.Load() != 4 {
		t.Fatalf("hits = %d, want 4", hits.Load())
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	httpDate := now.Add(45 * time.Second).Format(http.TimeFormat)
	farFuture := now.Add(2 * time.Hour).Format(http.TimeFormat)
	tests := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"", 0, false},
		{"0", 0, false},
		{"-5", 0, false},
		{"bogus", 0, false},
		{"12", 12 * time.Second, true},
		{"9999", maxRetryAfter, true},
		{httpDate, 45 * time.Second, true},
		{farFuture, maxRetryAfter, true},
	}
	for _, tt := range tests {
		got, ok := parseRetryAfter(tt.in, now)
		if ok != tt.ok || got != tt.want {
			t.Errorf("parseRetryAfter(%q) = %v, %v; want %v, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}
