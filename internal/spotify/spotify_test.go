package spotify

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAlbum(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		switch r.URL.Query().Get("url") {
		case "https://open.spotify.com/album/0tzfI6NFJqcJkWb23R3lRZ":
			w.Write([]byte(`{"title":"OK Computer OKNOTOK 1997 2017","thumbnail_url":"https://image-cdn-fa.spotifycdn.com/image/ab67616d00001e02ee58b8ce747da91d69a862cc"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := New("", "")
	c.BaseURL = srv.URL

	a, err := c.Album(context.Background(), "0tzfI6NFJqcJkWb23R3lRZ")
	if err != nil || a.Title != "OK Computer OKNOTOK 1997 2017" ||
		a.CoverURL != "https://image-cdn-fa.spotifycdn.com/image/ab67616d0000b273ee58b8ce747da91d69a862cc" {
		t.Fatalf("Album = %+v, %v", a, err)
	}
	for range 2 {
		if _, err := c.Album(context.Background(), "unknownunknownunknown1"); err != ErrNotFound {
			t.Errorf("unknown err = %v", err)
		}
	}
	c.Album(context.Background(), "0tzfI6NFJqcJkWb23R3lRZ")
	if hits != 2 {
		t.Errorf("%d requests, want 2 (cached, including not found)", hits)
	}
}

func TestCoverURLOnlyFromSpotify(t *testing.T) {
	for in, want := range map[string]string{
		"https://i.scdn.co/image/ab67616d00001e02abc":     "https://i.scdn.co/image/ab67616d0000b273abc",
		"https://image-cdn-ak.spotifycdn.com/image/x?y=1": "https://image-cdn-ak.spotifycdn.com/image/x",
		"http://i.scdn.co/image/x":                        "",
		"https://evil.example/image/x":                    "",
		"https://spotifycdn.com.evil.example/image/x":     "",
		"https://user@i.scdn.co/image/x":                  "",
		"javascript:alert(1)":                             "",
		"":                                                "",
	} {
		if got := coverURL(in); got != want {
			t.Errorf("coverURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeAPI serves the token endpoint and one album. status, if set, is
// returned for album requests instead.
type fakeAPI struct {
	tokens, albums int
	status         int
	expireNext     bool // answer the next album request with 401
}

func (f *fakeAPI) serve(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/token":
			id, secret, ok := r.BasicAuth()
			if !ok || id != "id" || secret != "secret" || r.FormValue("grant_type") != "client_credentials" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.tokens++
			w.Write([]byte(`{"access_token":"tok","token_type":"Bearer","expires_in":3600}`))
		case r.URL.Path == "/v1/albums/4m2880jivSbbyEGAKfITCa":
			f.albums++
			if r.Header.Get("Authorization") != "Bearer tok" || f.expireNext {
				f.expireNext = false
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if f.status != 0 {
				w.WriteHeader(f.status)
				return
			}
			w.Write([]byte(`{"name":"Random Access Memories","release_date":"2013-05-17",
"artists":[{"name":"Daft Punk"},{"name":"Guest"}],
"images":[{"url":"https://i.scdn.co/image/big","width":640},{"url":"https://evil.example/huge","width":2000},{"url":"https://i.scdn.co/image/small","width":64}]}`))
		case r.URL.Path == "/oembed":
			w.Write([]byte(`{"title":"From oEmbed","thumbnail_url":"https://i.scdn.co/image/thumb"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func newAPIClient(t *testing.T, f *fakeAPI) *Client {
	srv := f.serve(t)
	t.Cleanup(srv.Close)
	c := New("id", "secret")
	c.BaseURL, c.APIURL, c.AccountsURL = srv.URL, srv.URL+"/v1", srv.URL
	c.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	return c
}

func TestAlbumFromAPI(t *testing.T) {
	f := &fakeAPI{}
	c := newAPIClient(t, f)
	a, err := c.fetch(context.Background(), "4m2880jivSbbyEGAKfITCa")
	want := Album{Title: "Random Access Memories", Artist: "Daft Punk, Guest", Year: 2013, CoverURL: "https://i.scdn.co/image/big"}
	if err != nil || a != want {
		t.Fatalf("fetch = %+v, %v; want %+v", a, err, want)
	}
	c.fetch(context.Background(), "4m2880jivSbbyEGAKfITCa")
	if f.tokens != 1 {
		t.Errorf("%d token requests, want 1 (reused)", f.tokens)
	}
	// A revoked token is replaced on the next request.
	f.expireNext = true
	if a, _ := c.fetch(context.Background(), "4m2880jivSbbyEGAKfITCa"); a.Title != "From oEmbed" {
		t.Errorf("after 401 = %+v, want the oEmbed fallback", a)
	}
	if a, _ := c.fetch(context.Background(), "4m2880jivSbbyEGAKfITCa"); a.Artist != "Daft Punk, Guest" || f.tokens != 2 {
		t.Errorf("after new token = %+v, %d tokens", a, f.tokens)
	}
	if _, err := c.fetch(context.Background(), "unknownunknownunknown1"); err != ErrNotFound {
		t.Errorf("unknown album err = %v", err)
	}
}

func TestAPIFailureFallsBackToOEmbed(t *testing.T) {
	f := &fakeAPI{status: http.StatusForbidden} // e.g. the app owner's Premium lapsed
	c := newAPIClient(t, f)
	a, err := c.fetch(context.Background(), "4m2880jivSbbyEGAKfITCa")
	if err != nil || a.Title != "From oEmbed" || a.Artist != "" {
		t.Errorf("fetch = %+v, %v", a, err)
	}
	// Bad credentials too.
	c.ClientSecret = "wrong"
	c.token = ""
	if a, err := c.fetch(context.Background(), "4m2880jivSbbyEGAKfITCa"); err != nil || a.Title != "From oEmbed" {
		t.Errorf("bad credentials: %+v, %v", a, err)
	}
}
