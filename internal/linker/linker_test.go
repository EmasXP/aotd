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
	st      *store.Store
	linker  *Linker
	hits    int
	mbKnows map[string]bool // URLs MusicBrainz links to OK Computer
	srvURL  string
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
	e := &env{st: store.New(g), mbKnows: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.hits++
		switch {
		case r.URL.Path == "/release" && r.URL.Query().Get("release-group") == okComputer:
			w.Write([]byte(okComputerReleases))
		case r.URL.Path == "/release":
			w.Write([]byte(`{"release-count":0,"releases":[]}`))
		case r.URL.Path == "/url" && e.mbKnows[r.URL.Query().Get("resource")]:
			w.Write([]byte(`{"relations":[{"release":{"id":"30702389-5c67-4438-9ea0-2351c8de0f1d"}}]}`))
		case r.URL.Path == "/release/30702389-5c67-4438-9ea0-2351c8de0f1d":
			w.Write([]byte(`{"release-group":{"id":"` + okComputer + `","title":"OK Computer","first-release-date":"1997-05-21","artist-credit":[{"name":"Radiohead"}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	e.srvURL = srv.URL
	mb := musicbrainz.New("test")
	mb.BaseURL = srv.URL
	e.linker = New(e.st, mb)
	e.linker.Pause = 0
	return e
}

func (e *env) post(t *testing.T, username string, mbid *string, title string, link ...links.Link) *model.Post {
	t.Helper()
	u, err := e.st.CreateUser(username, username+"@example.com", username, "x")
	if err != nil {
		t.Fatal(err)
	}
	in := store.NewPost{MBID: mbid, Title: title, Artist: "Artist", CoverURL: "spotify-cover"}
	if len(link) > 0 {
		in.Link = &link[0]
	}
	p, err := e.st.CreatePost(u.ID, in)
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

var fresh = links.Link{Source: links.Spotify, ID: "1111111111111111111111"}

func TestPassMatchesLinkOnceMusicBrainzHasIt(t *testing.T) {
	e := newEnv(t)
	p := e.post(t, "alice", nil, "Fresh Indie", fresh)
	e.post(t, "bob", nil, "Garage Demo") // no link: nothing to look up
	ctx := context.Background()

	// MusicBrainz doesn't know the link yet.
	if n := e.linker.Pass(ctx); n != 1 {
		t.Fatalf("checked %d, want 1", n)
	}
	// Not asked again the same day...
	if n := e.linker.Pass(ctx); n != 0 {
		t.Errorf("checked again within a day: %d", n)
	}
	// ...but the next day, after someone added it to MusicBrainz.
	e.mbKnows[fresh.URL()] = true
	e.linker.MB = musicbrainz.New("test") // drop the cached "not found"
	e.linker.MB.BaseURL = e.srvURL
	tomorrow := time.Now().Add(25 * time.Hour)
	e.st.Now = func() time.Time { return tomorrow }
	if n := e.linker.Pass(ctx); n != 2 { // match, then the links for its MBID
		t.Fatalf("checked %d, want 2", n)
	}
	got, _ := e.st.PostByID(p.ID)
	if r := got.Release; r.MBID == nil || *r.MBID != okComputer || r.Title != "OK Computer" || r.CoverURL != "spotify-cover" {
		t.Errorf("release = %+v", r)
	}
	if ls, _ := e.st.Links(p.ReleaseID); len(ls) != 4 { // fresh + 3 from MusicBrainz
		t.Errorf("%d links, want 4", len(ls))
	}
}

func TestPassMergesIntoExistingRelease(t *testing.T) {
	e := newEnv(t)
	mbid := okComputer
	pa := e.post(t, "alice", nil, "OK Computer", fresh)
	pb := e.post(t, "bob", &mbid, "OK Computer")
	e.mbKnows[fresh.URL()] = true
	e.linker.Pass(context.Background())
	got, _ := e.st.PostByID(pa.ID)
	if got.ReleaseID != pb.ReleaseID {
		t.Errorf("Alice's post is on release %d, want Bob's %d", got.ReleaseID, pb.ReleaseID)
	}
	if r, err := e.st.ReleaseByLink(fresh); err != nil || r.ID != pb.ReleaseID {
		t.Errorf("link on %+v, %v", r, err)
	}
}

func TestReleasesWithoutMBIDBackOff(t *testing.T) {
	e := newEnv(t)
	e.post(t, "alice", nil, "Fresh Indie", fresh)
	base := time.Now()
	due := func(after time.Duration) int {
		e.st.Now = func() time.Time { return base.Add(after) }
		rs, err := e.st.ReleasesToCheck(10)
		if err != nil {
			t.Fatal(err)
		}
		return len(rs)
	}
	check := func(after time.Duration) {
		e.st.Now = func() time.Time { return base.Add(after) }
		rs, _ := e.st.ReleasesToCheck(10)
		e.st.MarkChecked(rs[0].ID)
	}
	if due(0) != 1 {
		t.Fatal("never-checked release not due")
	}
	check(0)
	if due(23*time.Hour) != 0 || due(25*time.Hour) != 1 {
		t.Error("young release isn't checked daily")
	}
	check(40 * 24 * time.Hour)
	if due(42*24*time.Hour) != 0 || due(48*24*time.Hour) != 1 {
		t.Error("old release isn't checked weekly")
	}
}
