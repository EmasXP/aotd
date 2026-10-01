// Package musicbrainz is a small client for the MusicBrainz web service,
// limited to what AOTD needs: searching and looking up album release groups.
//
// MusicBrainz asks clients for a meaningful User-Agent and at most one request
// per second; both are enforced here, and results are cached.
package musicbrainz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const DefaultBaseURL = "https://musicbrainz.org/ws/2"

var ErrNotFound = errors.New("musicbrainz: not found")

// Album is a MusicBrainz release group, flattened for display.
type Album struct {
	MBID   string
	Title  string
	Artist string
	Year   int
	Type   string // e.g. "Album"; secondary types like "Live" appended
}

// CoverURL is the Cover Art Archive front cover for a release group, 500px
// (sharp on phones and high-DPI screens). It may 404 if no art exists, so
// the UI needs a fallback.
func CoverURL(mbid string) string {
	return "https://coverartarchive.org/release-group/" + mbid + "/front-500"
}

type Client struct {
	BaseURL   string
	UserAgent string
	HTTP      *http.Client

	limiter *rate.Limiter
	mu      sync.Mutex
	cache   map[string]cacheEntry
	ttl     time.Duration
}

type cacheEntry struct {
	val     any
	expires time.Time
}

// New returns a client that identifies itself with contact (a URL or email).
func New(contact string) *Client {
	return &Client{
		BaseURL:   DefaultBaseURL,
		UserAgent: fmt.Sprintf("AOTD/0.1 ( %s )", contact),
		HTTP:      &http.Client{Timeout: 10 * time.Second},
		limiter:   rate.NewLimiter(rate.Every(time.Second), 1),
		cache:     map[string]cacheEntry{},
		ttl:       10 * time.Minute,
	}
}

var luceneSpecial = regexp.MustCompile(`([+\-!(){}\[\]^"~*?:\\/&|])`)

// escape makes user text safe inside a Lucene field query. Lowercasing
// neutralises AND/OR/NOT operators typed by the user.
func escape(text string) string {
	return luceneSpecial.ReplaceAllString(strings.ToLower(strings.TrimSpace(text)), `\$1`)
}

// buildQuery makes a Lucene query for albums by title and/or artist.
// Searching the fields separately ranks far better than free text, which
// tends to surface tribute albums first.
func buildQuery(album, artist string) string {
	var parts []string
	if a := escape(album); a != "" {
		parts = append(parts, "releasegroup:("+a+")")
	}
	if a := escape(artist); a != "" {
		parts = append(parts, "artist:("+a+")")
	}
	return strings.Join(append(parts, "primarytype:album"), " AND ")
}

// Search finds albums by title and/or artist.
func (c *Client) Search(ctx context.Context, album, artist string, limit int) ([]Album, error) {
	album, artist = strings.TrimSpace(album), strings.TrimSpace(artist)
	if album == "" && artist == "" {
		return nil, nil
	}
	key := "s:" + strconv.Itoa(limit) + ":" + strings.ToLower(album) + "\x00" + strings.ToLower(artist)
	if v, ok := c.cached(key); ok {
		return v.([]Album), nil
	}
	q := url.Values{"query": {buildQuery(album, artist)}, "fmt": {"json"}, "limit": {strconv.Itoa(limit)}}
	var resp struct {
		ReleaseGroups []releaseGroup `json:"release-groups"`
	}
	if err := c.get(ctx, "/release-group?"+q.Encode(), &resp); err != nil {
		return nil, err
	}
	albums := make([]Album, 0, len(resp.ReleaseGroups))
	for _, rg := range resp.ReleaseGroups {
		a := rg.album()
		albums = append(albums, a)
		c.store("l:"+a.MBID, a) // lets the lookup on submit skip a request
	}
	c.store(key, albums)
	return albums, nil
}

var mbidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ValidMBID reports whether s looks like a MusicBrainz ID.
func ValidMBID(s string) bool { return mbidRe.MatchString(s) }

// Lookup fetches a single release group by MBID.
func (c *Client) Lookup(ctx context.Context, mbid string) (Album, error) {
	if !ValidMBID(mbid) {
		return Album{}, ErrNotFound
	}
	key := "l:" + mbid
	if v, ok := c.cached(key); ok {
		return v.(Album), nil
	}
	var rg releaseGroup
	if err := c.get(ctx, "/release-group/"+mbid+"?inc=artist-credits&fmt=json", &rg); err != nil {
		return Album{}, err
	}
	a := rg.album()
	c.store(key, a)
	return a, nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	if err := c.limiter.Wait(ctx); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Accept", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusBadRequest:
		return ErrNotFound
	case res.StatusCode != http.StatusOK:
		return fmt.Errorf("musicbrainz: %s", res.Status)
	}
	return json.NewDecoder(http.MaxBytesReader(nil, res.Body, 4<<20)).Decode(out)
}

func (c *Client) cached(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.cache[key]
	if !ok || time.Now().After(e.expires) {
		return nil, false
	}
	return e.val, true
}

func (c *Client) store(key string, v any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if len(c.cache) > 5000 {
		for k, e := range c.cache {
			if now.After(e.expires) {
				delete(c.cache, k)
			}
		}
	}
	c.cache[key] = cacheEntry{val: v, expires: now.Add(c.ttl)}
}

type releaseGroup struct {
	ID               string   `json:"id"`
	Title            string   `json:"title"`
	FirstReleaseDate string   `json:"first-release-date"`
	PrimaryType      string   `json:"primary-type"`
	SecondaryTypes   []string `json:"secondary-types"`
	ArtistCredit     []struct {
		Name       string `json:"name"`
		JoinPhrase string `json:"joinphrase"`
	} `json:"artist-credit"`
}

func (rg releaseGroup) album() Album {
	var artist strings.Builder
	for _, ac := range rg.ArtistCredit {
		artist.WriteString(ac.Name)
		artist.WriteString(ac.JoinPhrase)
	}
	year := 0
	if len(rg.FirstReleaseDate) >= 4 {
		year, _ = strconv.Atoi(rg.FirstReleaseDate[:4])
	}
	typ := rg.PrimaryType
	if len(rg.SecondaryTypes) > 0 {
		typ += " · " + strings.Join(rg.SecondaryTypes, ", ")
	}
	return Album{MBID: rg.ID, Title: rg.Title, Artist: artist.String(), Year: year, Type: typ}
}
