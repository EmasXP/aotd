// Package spotify reads an album's details from Spotify. With app
// credentials it uses the Web API (title, artist, year, cover). Without them,
// or if the API fails, it falls back to the public oEmbed endpoint, which
// gives only the title and cover; the artist then comes from the person
// posting.
package spotify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/EmasXP/aotd/internal/links"
)

const (
	DefaultBaseURL     = "https://open.spotify.com"
	DefaultAPIURL      = "https://api.spotify.com/v1"
	DefaultAccountsURL = "https://accounts.spotify.com"
)

var ErrNotFound = errors.New("spotify: not found")

// Album is what Spotify tells about an album. Artist and Year are only
// known with API credentials.
type Album struct {
	Title    string
	Artist   string
	Year     int
	CoverURL string // empty if Spotify gave none we can use
}

type Client struct {
	BaseURL     string // oEmbed
	APIURL      string
	AccountsURL string
	HTTP        *http.Client
	Log         *slog.Logger
	// ClientID and ClientSecret enable the Web API, via the client
	// credentials flow (no user login). Both empty means oEmbed only.
	ClientID, ClientSecret string

	mu      sync.Mutex
	cache   map[string]cacheEntry
	token   string
	tokenTo time.Time
}

type cacheEntry struct {
	album   Album
	err     error
	expires time.Time
}

// New returns a client. Empty credentials mean oEmbed only.
func New(clientID, clientSecret string) *Client {
	return &Client{
		BaseURL: DefaultBaseURL, APIURL: DefaultAPIURL, AccountsURL: DefaultAccountsURL,
		HTTP: &http.Client{Timeout: 5 * time.Second}, Log: slog.Default(),
		ClientID: clientID, ClientSecret: clientSecret,
		cache: map[string]cacheEntry{},
	}
}

// HasAPI reports whether Web API credentials are configured.
func (c *Client) HasAPI() bool { return c.ClientID != "" && c.ClientSecret != "" }

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

// fetch uses the Web API when configured, and oEmbed otherwise or if the API
// fails for any reason but "not found" (e.g. the app owner's Premium lapsed).
func (c *Client) fetch(ctx context.Context, id string) (Album, error) {
	if c.HasAPI() {
		a, err := c.fetchAPI(ctx, id)
		if err == nil || errors.Is(err, ErrNotFound) {
			return a, err
		}
		c.Log.Warn("spotify api, falling back to oembed", "err", err)
	}
	return c.fetchOEmbed(ctx, id)
}

func (c *Client) fetchAPI(ctx context.Context, id string) (Album, error) {
	token, err := c.accessToken(ctx)
	if err != nil {
		return Album{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.APIURL+"/albums/"+url.PathEscape(id), nil)
	if err != nil {
		return Album{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	var a struct {
		Name        string `json:"name"`
		ReleaseDate string `json:"release_date"` // "1997-05-21", "1997-05" or "1997"
		Artists     []struct {
			Name string `json:"name"`
		} `json:"artists"`
		Images []struct {
			URL   string `json:"url"`
			Width int    `json:"width"`
		} `json:"images"`
	}
	if err := c.getJSON(req, &a); err != nil {
		if errors.Is(err, errUnauthorized) {
			c.mu.Lock()
			c.token = "" // fetch a new one next time
			c.mu.Unlock()
		}
		return Album{}, err
	}
	if a.Name = strings.TrimSpace(a.Name); a.Name == "" {
		return Album{}, ErrNotFound
	}
	out := Album{Title: a.Name}
	var artists []string
	for _, ar := range a.Artists {
		if n := strings.TrimSpace(ar.Name); n != "" {
			artists = append(artists, n)
		}
	}
	out.Artist = strings.Join(artists, ", ")
	if len(a.ReleaseDate) >= 4 {
		out.Year, _ = strconv.Atoi(a.ReleaseDate[:4])
	}
	best := 0
	for _, img := range a.Images {
		if u := coverURL(img.URL); u != "" && img.Width > best {
			out.CoverURL, best = u, img.Width
		}
	}
	return out, nil
}

// accessToken returns a cached client credentials token, fetching a new one
// shortly before it expires.
func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.token != "" && time.Now().Before(c.tokenTo) {
		t := c.token
		c.mu.Unlock()
		return t, nil
	}
	c.mu.Unlock()
	form := url.Values{"grant_type": {"client_credentials"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.AccountsURL+"/api/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(c.ClientID, c.ClientSecret)
	var t struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := c.getJSON(req, &t); err != nil {
		return "", fmt.Errorf("token: %w", err)
	}
	if t.AccessToken == "" {
		return "", errors.New("spotify: empty access token")
	}
	c.mu.Lock()
	c.token, c.tokenTo = t.AccessToken, time.Now().Add(time.Duration(t.ExpiresIn)*time.Second-time.Minute)
	c.mu.Unlock()
	return t.AccessToken, nil
}

var errUnauthorized = errors.New("spotify: unauthorized")

func (c *Client) getJSON(req *http.Request, out any) error {
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusBadRequest && req.Method == http.MethodGet:
		return ErrNotFound
	case res.StatusCode == http.StatusUnauthorized:
		return errUnauthorized
	case res.StatusCode != http.StatusOK:
		return fmt.Errorf("spotify: %s", res.Status)
	}
	return json.NewDecoder(http.MaxBytesReader(nil, res.Body, 1<<20)).Decode(out)
}

func (c *Client) fetchOEmbed(ctx context.Context, id string) (Album, error) {
	page := links.Link{Source: links.Spotify, ID: id}.URL()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/oembed?url="+url.QueryEscape(page), nil)
	if err != nil {
		return Album{}, err
	}
	var o struct {
		Title        string `json:"title"`
		ThumbnailURL string `json:"thumbnail_url"`
	}
	if err := c.getJSON(req, &o); err != nil {
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
