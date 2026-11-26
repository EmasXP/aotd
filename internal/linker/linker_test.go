package linker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/EmasXP/aotd/internal/cache"
	"github.com/EmasXP/aotd/internal/db"
	"github.com/EmasXP/aotd/internal/links"
	"github.com/EmasXP/aotd/internal/listenbrainz"
	"github.com/EmasXP/aotd/internal/model"
	"github.com/EmasXP/aotd/internal/musicbrainz"
	"github.com/EmasXP/aotd/internal/store"
)

const okComputer = "b1392450-e666-3926-a536-22c65f834433"

// okComputerReleases is a trimmed browse response: two Spotify editions
// (one listed twice), Deezer, and links AOTD doesn't show. The first is the
// original release, the second a later deluxe edition.
const okComputerReleases = `{"release-count":2,"releases":[
{"id":"1834eae1-741b-3c03-9ca5-0df3decb43ea","status":"Official","date":"1997-05-21","media":[{"track-count":12}],"relations":[
  {"url":{"resource":"https://open.spotify.com/album/6dVIqQ8qmQ5GBnJ9shOYGE"}},
  {"url":{"resource":"https://www.deezer.com/album/14879699"}},
  {"url":{"resource":"https://www.discogs.com/release/123"}}]},
{"id":"22222222-741b-3c03-9ca5-0df3decb43ea","status":"Official","date":"2009-08-25","media":[{"track-count":12},{"track-count":12}],"relations":[
  {"url":{"resource":"https://open.spotify.com/album/0tzfI6NFJqcJkWb23R3lRZ"}},
  {"url":{"resource":"https://open.spotify.com/album/6dVIqQ8qmQ5GBnJ9shOYGE"}}]}]}`

const radiohead = "a74b1b7f-71a5-4011-9441-d0b5e4122711"

// okComputerTracks is the original release: 12 tracks of 4m27s each.
var okComputerTracks = `{"media":[{"track-count":12,"tracks":[` +
	strings.TrimSuffix(strings.Repeat(`{"length":267000},`, 12), ",") + `]}]}`

type env struct {
	st      *store.Store
	linker  *Linker
	hits    int             // MusicBrainz requests
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
	c, err := cache.OpenBolt(filepath.Join(t.TempDir(), "cache.db"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	e := &env{st: store.New(g), mbKnows: map[string]bool{}}
	e.st.Cache = c
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/popularity/release-group" { // ListenBrainz
			w.Write([]byte(`[{"release_group_mbid":"` + okComputer + `","total_listen_count":1781830,"total_user_count":25280}]`))
			return
		}
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
		case r.URL.Path == "/release-group/"+okComputer:
			w.Write([]byte(`{"artist-credit":[{"name":"Radiohead","artist":{"id":"` + radiohead + `","name":"Radiohead"}}],
"genres":[{"name":"alternative rock","count":27},{"name":"art rock","count":15}]}`))
		case r.URL.Path == "/release/1834eae1-741b-3c03-9ca5-0df3decb43ea":
			w.Write([]byte(okComputerTracks))
		case r.URL.Path == "/artist/"+radiohead:
			w.Write([]byte(`{"id":"` + radiohead + `","name":"Radiohead","country":"GB","area":{"name":"United Kingdom"},"genres":[{"name":"art rock","count":9}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	e.srvURL = srv.URL
	mb := musicbrainz.New("test")
	mb.BaseURL = srv.URL
	mb.SetLimit(rate.Inf)
	lb := listenbrainz.New("test")
	lb.BaseURL = srv.URL
	lb.SetLimit(rate.Inf)
	e.linker = New(e.st, mb, lb)
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

	// OK Computer: releases, group, original release's length, Radiohead.
	// No Links: releases, group (not found).
	if n := e.linker.Pass(context.Background()); n != 2 || e.hits != 6 {
		t.Fatalf("checked %d releases with %d requests, want 2 and 6", n, e.hits)
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

func TestPassFetchesMetadataAndPopularity(t *testing.T) {
	e := newEnv(t)
	mbid := okComputer
	p := e.post(t, "alice", &mbid, "OK Computer")
	manual := e.post(t, "bob", nil, "Garage Demo")
	e.linker.Pass(context.Background())

	got, _ := e.st.PostByID(p.ID)
	r := got.Release
	if r.TrackCount != 12 || r.Length != 12*267 || r.MetaCheckedAt == nil {
		t.Errorf("release = %+v; want the original release's 12 tracks and length", r)
	}
	var genres []model.ReleaseGenre
	e.st.DB.Where("release_id = ?", r.ID).Order("votes DESC").Find(&genres)
	if len(genres) != 2 || genres[0].Genre != "alternative rock" || genres[0].Votes != 27 {
		t.Errorf("release genres = %+v", genres)
	}
	var credits []model.ReleaseArtist
	e.st.DB.Preload("Artist").Where("release_id = ?", r.ID).Find(&credits)
	if len(credits) != 1 {
		t.Fatalf("credits = %+v", credits)
	}
	if a := credits[0].Artist; a.MBID != radiohead || a.Country != "GB" || a.Area != "United Kingdom" || a.CheckedAt == nil {
		t.Errorf("artist = %+v", a)
	}
	var agenres int64
	e.st.DB.Model(&model.ArtistGenre{}).Where("artist_id = ?", credits[0].ArtistID).Count(&agenres)
	if agenres != 1 {
		t.Errorf("%d artist genres, want 1", agenres)
	}

	if got.Listeners == nil || *got.Listeners != 25280 || *got.Listens != 1781830 || got.PopularityAt == nil {
		t.Errorf("post popularity = %v listeners, %v listens at %v", got.Listeners, got.Listens, got.PopularityAt)
	}
	if m, _ := e.st.PostByID(manual.ID); m.PopularityAt != nil {
		t.Error("asked ListenBrainz about an album without an MBID")
	}

	// A release checked before metadata was fetched is due for it.
	e.st.DB.Model(&model.Release{}).Where("id = ?", r.ID).Update("meta_checked_at", nil)
	if rs, _ := e.st.ReleasesToCheck(10); len(rs) != 1 || rs[0].ID != r.ID {
		t.Errorf("releases to check = %+v, want the one without metadata", rs)
	}

	// Popularity is only asked within a week of posting.
	e.st.DB.Model(&model.Post{}).Where("id = ?", p.ID).Update("popularity_at", nil)
	later := time.Now().Add(store.PopularityWindow + time.Hour)
	e.st.Now = func() time.Time { return later }
	if ps, _ := e.st.PostsWithoutPopularity(10); len(ps) != 0 {
		t.Errorf("posts without popularity = %+v, want none after the window", ps)
	}
}

func TestRepresentative(t *testing.T) {
	rels := []musicbrainz.Release{
		{MBID: "bootleg", Status: "Bootleg", Date: "1996", Tracks: 10},
		{MBID: "deluxe", Status: "Official", Date: "1997-05-21", Tracks: 24},
		{MBID: "undated", Status: "Official", Tracks: 12},
		{MBID: "original", Status: "Official", Date: "1997-05-21", Tracks: 12},
		{MBID: "no-media", Status: "Official", Date: "1990"},
	}
	if r, ok := representative(rels); !ok || r.MBID != "original" {
		t.Errorf("representative = %+v, %v", r, ok)
	}
	if _, ok := representative(nil); ok {
		t.Error("representative of nothing")
	}
}

func TestKickNeverBlocks(t *testing.T) {
	var nilLinker *Linker
	nilLinker.Kick()
	l := New(nil, nil, nil)
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
	e.linker.MB.SetLimit(rate.Inf)
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
	check(store.LinkRefreshWindow - 24*time.Hour)
	if due(store.LinkRefreshWindow+8*24*time.Hour) != 0 {
		t.Error("unmatched release kept polling past LinkRefreshWindow")
	}
}

func TestRefreshStopsAfterWindowAndWakesOnRepost(t *testing.T) {
	e := newEnv(t)
	mbid := okComputer
	e.post(t, "alice", &mbid, "OK Computer")
	ctx := context.Background()

	// Initial pass fetches releases, group meta, release length, and artist (4 hits).
	if n := e.linker.Pass(ctx); n != 1 || e.hits != 4 {
		t.Fatalf("initial pass: %d releases, %d hits; want 1, 4", n, e.hits)
	}

	// 31 days later (within 6 months): refresh skips ReleaseLength (only 3 hits: releases, group meta, artist).
	base := time.Now()
	e.hits = 0
	e.st.Now = func() time.Time { return base.Add(store.LinkRefreshAge + time.Hour) }
	if n := e.linker.Pass(ctx); n != 1 || e.hits != 3 {
		t.Fatalf("refresh pass: %d releases, %d hits; want 1, 3 (skip ReleaseLength)", n, e.hits)
	}

	// Past 6 months: neither the release nor its artist is due.
	old := base.Add(store.LinkRefreshWindow + store.LinkRefreshAge + time.Hour)
	e.st.Now = func() time.Time { return old }
	if rs, _ := e.st.ReleasesToCheck(10); len(rs) != 0 {
		t.Errorf("releases due past window: %+v", rs)
	}
	if as, _ := e.st.ArtistsToCheck(10); len(as) != 0 {
		t.Errorf("artists due past window: %+v", as)
	}

	// Bob re-posts the old release: both release and artist wake back up.
	bob, err := e.st.CreateUser("bob", "bob@example.com", "bob", "x")
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.st.CreatePost(bob.ID, store.NewPost{MBID: &mbid, Title: "OK Computer", Artist: "Radiohead"})
	if err != nil {
		t.Fatal(err)
	}
	// GORM sets CreatedAt using time.Now(), so align the post's created_at with the simulated clock.
	e.st.DB.Model(&model.Post{}).Where("id = ?", p.ID).Update("created_at", old)

	if rs, _ := e.st.ReleasesToCheck(10); len(rs) != 1 {
		t.Errorf("releases due after repost = %d, want 1", len(rs))
	}
	if as, _ := e.st.ArtistsToCheck(10); len(as) != 1 {
		t.Errorf("artists due after repost = %d, want 1", len(as))
	}
}

func TestPassRespectsMaxPerPass(t *testing.T) {
	e := newEnv(t)
	e.linker.MaxPerPass = 2
	for i := range 3 {
		id := fmt.Sprintf("11111111-2222-3333-4444-%012d", i+1)
		e.post(t, fmt.Sprintf("user%d", i), &id, fmt.Sprintf("Album %d", i))
	}
	if n := e.linker.Pass(context.Background()); n != 2 {
		t.Fatalf("first pass checked %d, want 2", n)
	}
	if n := e.linker.Pass(context.Background()); n != 1 {
		t.Fatalf("second pass checked %d, want 1", n)
	}
}
