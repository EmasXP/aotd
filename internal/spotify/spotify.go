// Package spotify reads an album's title and cover from Spotify's public
// oEmbed endpoint. It needs no credentials, but doesn't give the artist or
// year; those come from MusicBrainz or the person posting.
package spotify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/EmasXP/aotd/internal/links"
)

const DefaultBaseURL = "https://open.spotify.com"

var ErrNotFound = errors.New("spotify: not found")

// Album is what oEmbed tells about an album.
type Album struct {
	Title    string
	CoverURL string // empty if Spotify gave none we can use
}

type Client struct {
	BaseURL string
	HTTP    *http.Client

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	album   Album
	err     error
	expires time.Time
}

func New() *Client {
	return &Client{BaseURL: DefaultBaseURL, HTTP: &http.Client{Timeout: 5 * time.Second}, cache: map[string]cacheEntry{}}
}

// Album looks up a Spotify album. Results, including not found, are cached
// for 10 minutes: the post form asks again on every keystroke.
func (c *Client) Album(ctx context.Context, id string) (Album, error) {
	c.mu.Lock()
	e, ok := c.cache[id]
	c.mu.Unlock()
	if ok && time.Now().Before(e.expires) {
		return e.album, e.err
	}
	a, err := c.fetch(ctx, id)
	if err == nil || errors.Is(err, ErrNotFound) {
		c.mu.Lock()
		if len(c.cache) > 1000 {
			clear(c.cache)
		}
		c.cache[id] = cacheEntry{a, err, time.Now().Add(10 * time.Minute)}
		c.mu.Unlock()
	}
	return a, err
}

func (c *Client) fetch(ctx context.Context, id string) (Album, error) {
	page := links.Link{Source: links.Spotify, ID: id}.URL()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/oembed?url="+url.QueryEscape(page), nil)
	if err != nil {
		return Album{}, err
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return Album{}, err
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusBadRequest:
		return Album{}, ErrNotFound
	case res.StatusCode != http.StatusOK:
		return Album{}, fmt.Errorf("spotify: %s", res.Status)
	}
	var o struct {
		Title        string `json:"title"`
		ThumbnailURL string `json:"thumbnail_url"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, res.Body, 1<<20)).Decode(&o); err != nil {
		return Album{}, err
	}
	if o.Title = strings.TrimSpace(o.Title); o.Title == "" {
		return Album{}, ErrNotFound
	}
	return Album{Title: o.Title, CoverURL: coverURL(o.ThumbnailURL)}, nil
}

// CoverHosts are where Spotify serves cover art from, for the CSP.
var CoverHosts = []string{"https://i.scdn.co", "https://*.spotifycdn.com"}

// coverURL accepts only Spotify image URLs, and asks for the 640px size
// instead of oEmbed's 300px.
func coverURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" ||
		(u.Host != "i.scdn.co" && !strings.HasSuffix(u.Host, ".spotifycdn.com")) {
		return ""
	}
	// Spotify image IDs start with a size code: 00001e02 is 300px, 0000b273 640px.
	u.Path = strings.Replace(u.Path, "/ab67616d00001e02", "/ab67616d0000b273", 1)
	u.RawQuery, u.Fragment = "", ""
	return u.String()
}
