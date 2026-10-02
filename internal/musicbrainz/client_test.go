package musicbrainz

import (
	"context"
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
	if _, err := c.LookupURL(context.Background(), "https://open.spotify.com/album/unknown"); err != ErrNotFound {
		t.Errorf("unknown URL err = %v", err)
	}
}

func TestReleaseGroupURLsPages(t *testing.T) {
	var offsets []string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/release" || r.URL.Query().Get("release-group") != "b1392450-e666-3926-a536-22c65f834433" {
			http.NotFound(w, r)
			return
		}
		off := r.URL.Query().Get("offset")
		offsets = append(offsets, off)
		w.Write([]byte(`{"release-count":150,"releases":[{"relations":[{"url":{"resource":"https://example.com/` + off + `"}},{"type":"other","artist":{}}]},{}]}`))
	})
	urls, err := c.ReleaseGroupURLs(context.Background(), "b1392450-e666-3926-a536-22c65f834433")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(offsets, ",") != "0,100" || strings.Join(urls, " ") != "https://example.com/0 https://example.com/100" {
		t.Errorf("offsets %v, urls %v", offsets, urls)
	}
}
