package linker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/EmasXP/aotd/internal/db"
	"github.com/EmasXP/aotd/internal/links"
	"github.com/EmasXP/aotd/internal/model"
	"github.com/EmasXP/aotd/internal/musicbrainz"
	"github.com/EmasXP/aotd/internal/store"
)

const okComputer = "b1392450-e666-3926-a536-22c65f834433"

// okComputerReleases is a trimmed browse response: two Spotify editions
// (one listed twice), Deezer, and links AOTD doesn't show.
const okComputerReleases = `{"release-count":2,"releases":[
{"relations":[
  {"url":{"resource":"https://open.spotify.com/album/6dVIqQ8qmQ5GBnJ9shOYGE"}},
  {"url":{"resource":"https://www.deezer.com/album/14879699"}},
  {"url":{"resource":"https://www.discogs.com/release/123"}}]},
{"relations":[
  {"url":{"resource":"https://open.spotify.com/album/0tzfI6NFJqcJkWb23R3lRZ"}},
  {"url":{"resource":"https://open.spotify.com/album/6dVIqQ8qmQ5GBnJ9shOYGE"}}]}]}`

type env struct {
	st     *store.Store
	linker *Linker
	hits   int
}

func newEnv(t *testing.T) *env {
	t.Helper()
	g, err := db.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name()))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(g); err != nil {
		t.Fatal(err)
	}
	e := &env{st: store.New(g)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.hits++
		if r.URL.Path == "/release" && r.URL.Query().Get("release-group") == okComputer {
			w.Write([]byte(okComputerReleases))
			return
		}
		w.Write([]byte(`{"release-count":0,"releases":[]}`))
	}))
	t.Cleanup(srv.Close)
	mb := musicbrainz.New("test")
	mb.BaseURL = srv.URL
	e.linker = New(e.st, mb)
	e.linker.Pause = 0
	return e
}

func (e *env) post(t *testing.T, username string, mbid *string, title string) *model.Post {
	t.Helper()
	u, err := e.st.CreateUser(username, username+"@example.com", username, "x")
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.st.CreatePost(u.ID, store.NewPost{MBID: mbid, Title: title, Artist: "Artist"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPassAddsLinksFromMusicBrainz(t *testing.T) {
	e := newEnv(t)
	mbid, other := okComputer, "11111111-2222-3333-4444-555555555555"
	p := e.post(t, "alice", &mbid, "OK Computer")
	e.post(t, "bob", &other, "No Links")
	e.post(t, "carol", nil, "Manual") // nothing to ask MusicBrainz yet

	if n := e.linker.Pass(context.Background()); n != 2 || e.hits != 2 {
		t.Fatalf("checked %d releases with %d requests, want 2 and 2", n, e.hits)
	}
	var got []links.Link
	var rows []model.ReleaseLink
	e.st.DB.Where("release_id = ?", p.ReleaseID).Order("id").Find(&rows)
	for _, r := range rows {
		got = append(got, r.Link())
	}
	want := []links.Link{{Source: links.Spotify, ID: "6dVIqQ8qmQ5GBnJ9shOYGE"}, {Source: links.Deezer, ID: "14879699"}, {Source: links.Spotify, ID: "0tzfI6NFJqcJkWb23R3lRZ"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("links = %v, want %v", got, want)
	}

	// The wall shows one per source, in display order.
	it, err := e.st.Item(0, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(it.Links) != 2 || it.Links[0].ExternalID != "6dVIqQ8qmQ5GBnJ9shOYGE" || it.Links[1].Source != links.Deezer {
		t.Errorf("shown links = %+v", it.Links)
	}

	// Nothing is due again until the refresh age has passed.
	if n := e.linker.Pass(context.Background()); n != 0 {
		t.Errorf("second pass checked %d", n)
	}
	later := time.Now().Add(store.LinkRefreshAge + time.Hour)
	e.st.Now = func() time.Time { return later }
	if n := e.linker.Pass(context.Background()); n != 2 {
		t.Errorf("refresh pass checked %d, want 2", n)
	}
	var count int64
	e.st.DB.Model(&model.ReleaseLink{}).Count(&count)
	if count != 3 {
		t.Errorf("%d links after refresh, want 3 (no duplicates)", count)
	}
}

func TestKickNeverBlocks(t *testing.T) {
	var nilLinker *Linker
	nilLinker.Kick()
	l := New(nil, nil)
	l.Kick()
	l.Kick()
}
