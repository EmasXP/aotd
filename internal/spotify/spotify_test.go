package spotify

import (
	"context"
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
	c := New()
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
