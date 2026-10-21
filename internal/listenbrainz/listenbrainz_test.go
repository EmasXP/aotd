package listenbrainz

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/time/rate"
)

func TestPopularity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			MBIDs []string `json:"release_group_mbids"`
		}
		if r.Method != http.MethodPost || r.URL.Path != "/popularity/release-group" || json.NewDecoder(r.Body).Decode(&in) != nil || len(in.MBIDs) != 2 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Write([]byte(`[{"release_group_mbid":"known","total_listen_count":1781830,"total_user_count":25280},
{"release_group_mbid":"unknown","total_listen_count":null,"total_user_count":null}]`))
	}))
	defer srv.Close()
	c := New("test")
	c.BaseURL = srv.URL
	c.limiter = rate.NewLimiter(rate.Inf, 1)

	got, err := c.Popularity(context.Background(), []string{"known", "unknown"})
	if err != nil {
		t.Fatal(err)
	}
	if p := got["known"]; p.Listens == nil || *p.Listens != 1781830 || *p.Listeners != 25280 {
		t.Errorf("known = %+v", p)
	}
	if p, ok := got["unknown"]; !ok || p.Listens != nil || p.Listeners != nil {
		t.Errorf("unknown = %+v, %v", p, ok)
	}
	if _, err := c.Popularity(context.Background(), make([]string, MaxBatch+1)); err == nil {
		t.Error("oversized batch accepted")
	}
}
