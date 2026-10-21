// Package listenbrainz asks ListenBrainz how popular albums are. It needs
// no account: the popularity API is public.
package listenbrainz

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

const DefaultBaseURL = "https://api.listenbrainz.org/1"

// MaxBatch is how many albums Popularity asks about per request.
const MaxBatch = 100

// Popularity is how many times an album was listened to on ListenBrainz,
// and by how many users. Nil when ListenBrainz has no data.
type Popularity struct {
	Listens   *int
	Listeners *int
}

type Client struct {
	BaseURL   string
	UserAgent string
	HTTP      *http.Client

	limiter *rate.Limiter
}

// New returns a client that identifies itself with contact (a URL or email).
func New(contact string) *Client {
	return &Client{
		BaseURL:   DefaultBaseURL,
		UserAgent: fmt.Sprintf("AOTD/0.1 ( %s )", contact),
		HTTP:      &http.Client{Timeout: 10 * time.Second},
		// ListenBrainz allows a burst of 30 per window; one a second is
		// plenty for background work.
		limiter: rate.NewLimiter(rate.Every(time.Second), 1),
	}
}

// SetLimit changes the rate limit, e.g. to rate.Inf against a fake server.
func (c *Client) SetLimit(l rate.Limit) { c.limiter.SetLimit(l) }

// Popularity returns the popularity of release groups by MBID, at most
// MaxBatch at a time. Albums ListenBrainz doesn't know get a zero
// Popularity.
func (c *Client) Popularity(ctx context.Context, mbids []string) (map[string]Popularity, error) {
	if len(mbids) > MaxBatch {
		return nil, fmt.Errorf("listenbrainz: %d albums, at most %d per request", len(mbids), MaxBatch)
	}
	out := make(map[string]Popularity, len(mbids))
	if len(mbids) == 0 {
		return out, nil
	}
	body, err := json.Marshal(map[string][]string{"release_group_mbids": mbids})
	if err != nil {
		return nil, err
	}
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/popularity/release-group", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return nil, fmt.Errorf("listenbrainz: %s: %s", res.Status, strings.TrimSpace(string(b)))
	}
	var rows []struct {
		MBID      string `json:"release_group_mbid"`
		Listens   *int   `json:"total_listen_count"`
		Listeners *int   `json:"total_user_count"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, res.Body, 1<<20)).Decode(&rows); err != nil {
		return nil, err
	}
	for _, id := range mbids {
		out[id] = Popularity{}
	}
	for _, r := range rows {
		if _, ok := out[r.MBID]; ok {
			out[r.MBID] = Popularity{Listens: r.Listens, Listeners: r.Listeners}
		}
	}
	return out, nil
}
