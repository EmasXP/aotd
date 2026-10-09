package web

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/EmasXP/aotd/internal/auth"
	"github.com/EmasXP/aotd/internal/cache"
	"github.com/EmasXP/aotd/internal/db"
	"github.com/EmasXP/aotd/internal/musicbrainz"
	"github.com/EmasXP/aotd/internal/store"
)

const okComputer = "b1392450-e666-3926-a536-22c65f834433"

const (
	// spotifyOnMB is linked to OK Computer on MusicBrainz; spotifyNew isn't.
	spotifyOnMB = "https://open.spotify.com/album/6dVIqQ8qmQ5GBnJ9shOYGE"
	spotifyNew  = "https://open.spotify.com/album/1111111111111111111111"
)

// fakeMB serves just enough of the MusicBrainz API.
func fakeMB() *httptest.Server {
	rg := `{"id":"` + okComputer + `","title":"OK Computer","first-release-date":"1997-05-21","primary-type":"Album","artist-credit":[{"name":"Radiohead"}]}`
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/release-group":
			fmt.Fprintf(w, `{"release-groups":[%s]}`, rg)
		case r.URL.Path == "/release-group/"+okComputer:
			w.Write([]byte(rg))
		case r.URL.Path == "/url" && r.URL.Query().Get("resource") == spotifyOnMB:
			w.Write([]byte(`{"relations":[{"release":{"id":"30702389-5c67-4438-9ea0-2351c8de0f1d"}}]}`))
		case r.URL.Path == "/release/30702389-5c67-4438-9ea0-2351c8de0f1d":
			fmt.Fprintf(w, `{"release-group":%s}`, rg)
		default:
			http.NotFound(w, r)
		}
	}))
}

// fakeSpotify serves oEmbed and the Web API for spotifyNew.
func fakeSpotify() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oembed" && r.URL.Query().Get("url") == spotifyNew:
			w.Write([]byte(`{"title":"Fresh Indie","thumbnail_url":"https://i.scdn.co/image/ab67616d00001e02cafe"}`))
		case r.URL.Path == "/api/token":
			w.Write([]byte(`{"access_token":"tok","expires_in":3600}`))
		case r.URL.Path == "/v1/albums/1111111111111111111111":
			w.Write([]byte(`{"name":"Fresh Indie","release_date":"2026-09-01","artists":[{"name":"The Newcomers"}],"images":[{"url":"https://i.scdn.co/image/big","width":640}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

type env struct {
	t     *testing.T
	srv   *httptest.Server
	store *store.Store
	web   *Server
	spURL string // fake Spotify
}

func newEnv(t *testing.T) *env {
	t.Helper()
	g, err := db.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_")))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(g); err != nil {
		t.Fatal(err)
	}
	mbSrv := fakeMB()
	t.Cleanup(mbSrv.Close)
	mb := musicbrainz.New("test")
	mb.BaseURL = mbSrv.URL
	st := store.New(g)
	// A real cache, so a missed invalidation fails tests as a stale read.
	c, err := cache.OpenBolt(filepath.Join(t.TempDir(), "cache.db"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	st.Cache = c
	s, err := New(st, mb, t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	spSrv := fakeSpotify()
	t.Cleanup(spSrv.Close)
	s.Spotify.BaseURL = spSrv.URL
	s.Params = auth.Params{Memory: 8 * 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32} // fast tests
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, store: st, web: s, spURL: spSrv.URL}
}

// client is a browser-like user with a cookie jar.
type client struct {
	e *env
	c *http.Client
}

func (e *env) client() *client {
	jar, _ := cookiejar.New(nil)
	return &client{e: e, c: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}}
}

func (c *client) do(method, path string, form url.Values, headers ...string) (int, string) {
	c.e.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, c.e.srv.URL+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := c.c.Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

// htmx sends a fragment request the way htmx would.
func (c *client) htmx(method, path string, form url.Values) (int, string) {
	return c.do(method, path, form, "HX-Request", "true")
}

func (e *env) signup(username string) *client {
	c := e.client()
	code, body := c.do("POST", "/signup", url.Values{
		"username": {username}, "email": {username + "@example.com"},
		"password": {"a long enough password"}, "password_confirm": {"a long enough password"},
	})
	if code != http.StatusSeeOther {
		e.t.Fatalf("signup %s: %d %s", username, code, body)
	}
	return c
}

func mustContain(t *testing.T, body string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(body, s) {
			t.Errorf("response missing %q", s)
		}
	}
}

func TestAuthFlow(t *testing.T) {
	e := newEnv(t)
	anon := e.client()
	if code, _ := anon.do("GET", "/", nil); code != http.StatusSeeOther {
		t.Errorf("anonymous wall: %d, want redirect", code)
	}

	alice := e.signup("alice")
	code, body := alice.do("GET", "/", nil)
	if code != 200 {
		t.Fatalf("wall after signup: %d", code)
	}
	mustContain(t, body, "What's your album of the day?")

	// Weak and mismatched passwords are rejected.
	c := e.client()
	code, body = c.do("POST", "/signup", url.Values{"username": {"bob"}, "email": {"bob@example.com"}, "password": {"1234567890"}, "password_confirm": {"1234567890"}})
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, "too common") {
		t.Errorf("common password: %d", code)
	}
	code, _ = c.do("POST", "/signup", url.Values{"username": {"alice"}, "email": {"x@example.com"}, "password": {"another fine password"}, "password_confirm": {"another fine password"}})
	if code != http.StatusConflict {
		t.Errorf("duplicate username: %d", code)
	}

	// Same message for unknown user and wrong password.
	_, unknown := c.do("POST", "/login", url.Values{"login": {"nobody"}, "password": {"whatever whatever"}})
	_, wrong := c.do("POST", "/login", url.Values{"login": {"alice"}, "password": {"whatever whatever"}})
	if !strings.Contains(unknown, "Wrong username/email or password.") || !strings.Contains(wrong, "Wrong username/email or password.") {
		t.Error("login errors should be identical")
	}
	// Login by email works, and an open redirect via ?next= is refused.
	code, _ = c.do("POST", "/login", url.Values{"login": {"ALICE@example.com"}, "password": {"a long enough password"}, "next": {"//evil.example"}})
	if code != http.StatusSeeOther {
		t.Fatalf("login by email: %d", code)
	}

	// Logout kills the session server-side.
	alice.do("POST", "/logout", nil)
	if code, _ := alice.do("GET", "/", nil); code != http.StatusSeeOther {
		t.Errorf("after logout: %d", code)
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t)
	e.signup("alice")
	c := e.client()
	var last int
	for range 7 {
		last, _ = c.do("POST", "/login", url.Values{"login": {"alice"}, "password": {"wrong password!"}})
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("after 7 attempts: %d, want 429", last)
	}
}

func TestCrossOriginPostRejected(t *testing.T) {
	e := newEnv(t)
	alice := e.signup("alice")
	code, _ := alice.do("POST", "/posts", url.Values{"mode": {"manual"}, "title": {"X"}, "artist": {"Y"}},
		"Sec-Fetch-Site", "cross-site")
	if code != http.StatusForbidden {
		t.Errorf("cross-site POST: %d, want 403", code)
	}
}

func TestSecurityHeadersAndCookie(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	res, err := http.Get(e.srv.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("CSP = %q", csp)
	}
	if res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff")
	}
	req, _ := http.NewRequest("POST", e.srv.URL+"/signup", strings.NewReader(url.Values{
		"username": {"carol"}, "email": {"carol@example.com"},
		"password": {"a long enough password"}, "password_confirm": {"a long enough password"},
	}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err = c.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	cookie := res.Header.Get("Set-Cookie")
	for _, attr := range []string{"aotd_session=", "HttpOnly", "SameSite=Lax"} {
		if !strings.Contains(cookie, attr) {
			t.Errorf("session cookie %q missing %s", cookie, attr)
		}
	}
}

func TestPostCheckInCommentFlow(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.signup("alice"), e.signup("bob")

	// MusicBrainz search returns pickable results.
	code, body := alice.htmx("GET", "/mb/search?title=ok+computer&artist=radiohead", nil)
	if code != 200 {
		t.Fatal(code)
	}
	mustContain(t, body, `value="`+okComputer+`"`, "OK Computer", "Radiohead")

	// Post with an MBID; metadata comes from the server-side lookup, not the form.
	code, _ = alice.do("POST", "/posts", url.Values{"mode": {"mb"}, "mbid": {okComputer}, "title": {"spoofed"}, "note": {"Classic."}})
	if code != http.StatusSeeOther {
		t.Fatalf("create post: %d", code)
	}
	p, err := e.store.PostOn(1, e.store.Today())
	if err != nil {
		t.Fatal(err)
	}
	if r := p.Release; r.Title != "OK Computer" || r.MBID == nil || *r.MBID != okComputer || !strings.HasSuffix(r.CoverURL, "/front-500") {
		t.Errorf("stored release = %+v", r)
	}
	// Second post the same day is refused.
	code, body = alice.do("POST", "/posts", url.Values{"mode": {"manual"}, "title": {"Other"}, "artist": {"Band"}})
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, "already posted") {
		t.Errorf("second post: %d", code)
	}
	// Bob posts manually, without an MBID.
	code, _ = bob.do("POST", "/posts", url.Values{"mode": {"manual"}, "title": {"Garage Demo"}, "artist": {"Bob's Band"}, "year": {"2024"}})
	if code != http.StatusSeeOther {
		t.Fatalf("manual post: %d", code)
	}
	if bp, _ := e.store.PostOn(2, e.store.Today()); bp.Release.MBID != nil || bp.Release.Year != 2024 {
		t.Errorf("manual release = %+v", bp.Release)
	}

	id := fmt.Sprint(p.ID)
	// No check-in on your own post.
	if code, _ := alice.htmx("POST", "/posts/"+id+"/checkin", nil); code != http.StatusForbidden {
		t.Errorf("own check-in: %d", code)
	}
	code, body = bob.htmx("POST", "/posts/"+id+"/checkin", nil)
	if code != 200 || !strings.Contains(body, "Listened") {
		t.Errorf("check-in: %d %s", code, body)
	}
	// It shows on Bob's profile check-in tab.
	_, body = alice.do("GET", "/u/bob?tab=checkins", nil)
	mustContain(t, body, "OK Computer")

	// Comments: reply-to-reply is flattened to one level.
	bob.htmx("POST", "/posts/"+id+"/comments", url.Values{"body": {"Great pick"}})
	alice.htmx("POST", "/posts/"+id+"/comments", url.Values{"body": {"Thanks!"}, "parent_id": {"1"}})
	code, body = bob.htmx("POST", "/posts/"+id+"/comments", url.Values{"body": {"<script>x</script>"}, "parent_id": {"2"}})
	if code != 200 {
		t.Fatalf("reply: %d", code)
	}
	if strings.Contains(body, "<script>x") {
		t.Error("comment body not escaped")
	}
	threads, _ := e.store.Comments(p.ID)
	if len(threads) != 1 || len(threads[0].Replies) != 2 {
		t.Errorf("threads = %+v", threads)
	}
	// Only the author can delete.
	if code, _ := alice.htmx("DELETE", "/comments/1", nil); code != http.StatusForbidden {
		t.Errorf("delete other's comment: %d", code)
	}
}

func TestFollowAndWall(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.signup("alice"), e.signup("bob")
	bob.do("POST", "/posts", url.Values{"mode": {"manual"}, "title": {"Bobs Album"}, "artist": {"B"}})

	if _, body := alice.do("GET", "/", nil); strings.Contains(body, "Bobs Album") {
		t.Error("unfollowed user's post on wall")
	}
	code, body := alice.htmx("POST", "/u/bob/follow", nil)
	if code != 200 || !strings.Contains(body, "Following") {
		t.Errorf("follow: %d %s", code, body)
	}
	_, body = alice.do("GET", "/", nil)
	mustContain(t, body, "Bobs Album")
	if code, _ := alice.htmx("POST", "/u/alice/follow", nil); code != http.StatusUnprocessableEntity {
		t.Errorf("self-follow: %d", code)
	}
}

func TestGroupsAndSearch(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.signup("alice"), e.signup("bob")
	code, _ := alice.do("POST", "/groups", url.Values{"name": {"Masters of FTT"}})
	if code != http.StatusSeeOther {
		t.Fatalf("create group: %d", code)
	}
	bob.do("POST", "/posts", url.Values{"mode": {"manual"}, "title": {"Bobs Album"}, "artist": {"B"}})
	bob.htmx("POST", "/g/masters-of-ftt/members", nil)
	_, body := alice.do("GET", "/g/masters-of-ftt", nil)
	mustContain(t, body, "Bobs Album", "2 members")

	if code, _ := bob.do("POST", "/g/masters-of-ftt/edit", url.Values{"name": {"Mine now"}}); code != http.StatusForbidden {
		t.Errorf("non-owner edit: %d", code)
	}
	_, body = alice.do("GET", "/search?q=master", nil)
	mustContain(t, body, "Masters of FTT")
	_, body = alice.do("GET", "/search?q=bo", nil)
	mustContain(t, body, "@bob")
}

func TestAvatarAndPasswordChange(t *testing.T) {
	e := newEnv(t)
	alice := e.signup("alice")

	var buf bytes.Buffer
	png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 300, 200)))
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("avatar", "me.png")
	fw.Write(buf.Bytes())
	mw.Close()
	req, _ := http.NewRequest("POST", e.srv.URL+"/settings/avatar", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, err := alice.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("avatar upload: %d", res.StatusCode)
	}
	u, _ := e.store.UserByUsername("alice")
	if u.AvatarPath == "" {
		t.Fatal("avatar not saved")
	}
	code, img := alice.do("GET", "/avatars/"+u.AvatarPath, nil)
	if code != 200 || !strings.HasPrefix(img, "\xff\xd8") {
		t.Errorf("avatar fetch: %d", code)
	}
	// Removing the picture deletes the file.
	alice.do("POST", "/settings/avatar/delete", nil)
	if code, _ := alice.do("GET", "/avatars/"+u.AvatarPath, nil); code != http.StatusNotFound {
		t.Errorf("removed avatar still served: %d", code)
	}
	if code, _ := alice.do("GET", "/avatars/..%2faotd.db", nil); code != http.StatusNotFound {
		t.Errorf("path traversal: %d", code)
	}

	// Password change logs out other sessions but keeps this one.
	other := e.client()
	other.do("POST", "/login", url.Values{"login": {"alice"}, "password": {"a long enough password"}})
	code, _ = alice.do("POST", "/settings/password", url.Values{"current": {"wrong one"}, "new": {"brand new password"}, "confirm": {"brand new password"}})
	if code != http.StatusUnprocessableEntity {
		t.Errorf("wrong current password: %d", code)
	}
	code, _ = alice.do("POST", "/settings/password", url.Values{"current": {"a long enough password"}, "new": {"brand new password"}, "confirm": {"brand new password"}})
	if code != http.StatusSeeOther {
		t.Fatalf("password change: %d", code)
	}
	if code, _ := alice.do("GET", "/settings", nil); code != 200 {
		t.Errorf("current session after change: %d", code)
	}
	if code, _ := other.do("GET", "/settings", nil); code != http.StatusSeeOther {
		t.Errorf("other session after change: %d, want logged out", code)
	}
	if code, _ := e.client().do("POST", "/login", url.Values{"login": {"alice"}, "password": {"brand new password"}}); code != http.StatusSeeOther {
		t.Errorf("login with new password: %d", code)
	}
}

func TestMain(m *testing.M) {
	// Keep test output readable: drop per-request logs.
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

func TestPostFromLinkMusicBrainzKnows(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.signup("alice"), e.signup("bob")
	// The link resolves to the MusicBrainz album, already picked.
	_, body := alice.htmx("GET", "/mb/search?"+url.Values{"title": {spotifyOnMB + "?si=x"}}.Encode(), nil)
	mustContain(t, body, `value="`+okComputer+`" required checked`, "matched from this Spotify link")
	// Posting without a pick works: the server resolves the link itself.
	if code, body := alice.do("POST", "/posts", url.Values{"mode": {"mb"}, "title": {spotifyOnMB}}); code != http.StatusSeeOther {
		t.Fatalf("post: %d %s", code, body)
	}
	p, _ := e.store.PostOn(1, e.store.Today())
	if p.Release.MBID == nil || *p.Release.MBID != okComputer || p.LinkID == nil {
		t.Fatalf("post = %+v", p)
	}
	// Bob searches MusicBrainz for it; his post shows Alice's link.
	bob.do("POST", "/posts", url.Values{"mode": {"mb"}, "mbid": {okComputer}})
	_, body = bob.do("GET", "/", nil)
	mustContain(t, body, `href="`+spotifyOnMB+`"`, "Spotify", `href="spotify:album:6dVIqQ8qmQ5GBnJ9shOYGE"`, "app-launcher")
}

func TestPostFromNewLinkThenMatch(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.signup("alice"), e.signup("bob")
	// Unknown everywhere: the link's own title, and candidates nobody has picked.
	_, body := alice.htmx("GET", "/mb/search?"+url.Values{"title": {spotifyNew}}.Encode(), nil)
	mustContain(t, body, "Fresh Indie", "From Spotify", "Type the artist above", "Is it one of these on MusicBrainz?",
		`value="`+okComputer+`" required >`, "None of these", `value="" checked`)
	// "Post AOTD" without a match posts the link's album, once there's an artist.
	if code, body := alice.do("POST", "/posts", url.Values{"mode": {"mb"}, "title": {spotifyNew}, "mbid": {""}}); code != http.StatusUnprocessableEntity || !strings.Contains(body, "Type the artist") {
		t.Fatalf("post without artist: %d", code)
	}
	if code, body := alice.do("POST", "/posts", url.Values{"mode": {"mb"}, "title": {spotifyNew}, "mbid": {""}, "artist": {"Band"}}); code != http.StatusSeeOther {
		t.Fatalf("post: %d %s", code, body)
	}
	p, _ := e.store.PostOn(1, e.store.Today())
	if r := p.Release; r.Title != "Fresh Indie" || r.Artist != "Band" || r.MBID != nil || r.CoverURL != "https://i.scdn.co/image/ab67616d0000b273cafe" {
		t.Fatalf("release = %+v", r)
	}
	// Bob pastes the same link: AOTD knows it, and offers to match it.
	_, body = bob.htmx("GET", "/mb/search?"+url.Values{"title": {spotifyNew}}.Encode(), nil)
	mustContain(t, body, "Already on AOTD", "None of these", `value="" checked`)
	if code, body := bob.do("POST", "/posts", url.Values{"mode": {"mb"}, "title": {spotifyNew}, "mbid": {okComputer}}); code != http.StatusSeeOther {
		t.Fatalf("bob's post: %d %s", code, body)
	}
	bp, _ := e.store.PostOn(2, e.store.Today())
	ap, _ := e.store.PostByID(p.ID)
	if bp.ReleaseID != p.ReleaseID || ap.Release.MBID == nil || *ap.Release.MBID != okComputer {
		t.Errorf("after match: alice's release %+v, bob's release id %d", ap.Release, bp.ReleaseID)
	}
}

func TestMatchManualPostLater(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.signup("alice"), e.signup("bob")
	alice.do("POST", "/posts", url.Values{"mode": {"manual"}, "title": {"OK Computr"}, "artist": {"Radiohead"}})
	p, _ := e.store.PostOn(1, e.store.Today())
	id := fmt.Sprint(p.ID)

	_, body := alice.do("GET", "/posts/"+id, nil)
	mustContain(t, body, "Is this album on MusicBrainz now?")
	if _, body := bob.do("GET", "/posts/"+id, nil); strings.Contains(body, "Is this album on MusicBrainz now?") {
		t.Error("match form shown to someone else")
	}
	if code, _ := bob.do("POST", "/posts/"+id+"/release", url.Values{"mbid": {okComputer}}); code != http.StatusForbidden {
		t.Errorf("bob matching alice's post: %d", code)
	}
	if code, _ := alice.do("POST", "/posts/"+id+"/release", url.Values{"mbid": {"not-an-mbid"}}); code != http.StatusUnprocessableEntity {
		t.Errorf("bad mbid: %d", code)
	}
	if code, body := alice.do("POST", "/posts/"+id+"/release", url.Values{"mbid": {okComputer}}); code != http.StatusSeeOther {
		t.Fatalf("match: %d %s", code, body)
	}
	got, _ := e.store.PostByID(p.ID)
	if r := got.Release; r.MBID == nil || *r.MBID != okComputer || r.Title != "OK Computer" {
		t.Errorf("release = %+v", r)
	}
	// Once linked, it stays linked.
	if code, _ := alice.do("POST", "/posts/"+id+"/release", url.Values{"mbid": {okComputer}}); code != http.StatusUnprocessableEntity {
		t.Errorf("second match: %d", code)
	}
	_, body = alice.do("GET", "/posts/"+id, nil)
	mustContain(t, body, "musicbrainz.org/release-group/"+okComputer)
	if strings.Contains(body, "Is this album on MusicBrainz now?") {
		t.Error("match form still shown")
	}
}

func TestPostFromNewLinkWithSpotifyAPI(t *testing.T) {
	e := newEnv(t)
	sp := e.web.Spotify
	sp.ClientID, sp.ClientSecret = "id", "secret"
	sp.APIURL, sp.AccountsURL = e.spURL+"/v1", e.spURL
	alice := e.signup("alice")
	_, body := alice.htmx("GET", "/mb/search?"+url.Values{"title": {spotifyNew}}.Encode(), nil)
	mustContain(t, body, "The Newcomers", "2026 · From Spotify")
	if strings.Contains(body, "Add the artist above") {
		t.Error("asks for the artist although Spotify gave it")
	}
	mustContain(t, body, "Ready to post.")
	// "Post AOTD" needs no typing: Spotify's artist fills in.
	if code, body := alice.do("POST", "/posts", url.Values{"mode": {"mb"}, "title": {spotifyNew}, "mbid": {""}}); code != http.StatusSeeOther {
		t.Fatalf("post: %d %s", code, body)
	}
	p, _ := e.store.PostOn(1, e.store.Today())
	if r := p.Release; r.Artist != "The Newcomers" || r.Year != 2026 || r.CoverURL != "https://i.scdn.co/image/big" {
		t.Errorf("release = %+v", r)
	}
}

func TestPostNewLinkAsTyped(t *testing.T) {
	e := newEnv(t)
	alice := e.signup("alice")
	// "Post without MusicBrainz" uses the link's title and the typed artist and year.
	if code, body := alice.do("POST", "/posts", url.Values{"mode": {"manual"}, "title": {spotifyNew}, "artist": {"Band"}, "year": {"2025"}}); code != http.StatusSeeOther {
		t.Fatalf("post: %d %s", code, body)
	}
	p, _ := e.store.PostOn(1, e.store.Today())
	if r := p.Release; r.Title != "Fresh Indie" || r.Artist != "Band" || r.Year != 2025 || r.MBID != nil {
		t.Errorf("release = %+v", r)
	}
}

func TestNotificationsFlow(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.signup("alice"), e.signup("bob")
	alice.do("POST", "/posts", url.Values{"mode": {"manual"}, "title": {"Fresh Indie"}, "artist": {"Band"}})
	p, err := e.store.PostOn(1, e.store.Today())
	if err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprint(p.ID)

	_, body := alice.do("GET", "/", nil)
	if strings.Contains(body, `class="badge"`) {
		t.Error("badge before any notification")
	}

	// A mention of the owner is one notification, not two.
	bob.htmx("POST", "/posts/"+id+"/comments", url.Values{"body": {"@alice <b>nice</b>"}})
	bob.htmx("POST", "/posts/"+id+"/checkin", nil)
	_, body = alice.do("GET", "/", nil)
	mustContain(t, body, `<span class="badge">2</span>`)

	_, body = alice.do("GET", "/notifications", nil)
	mustContain(t, body, "mentioned you on", "checked in on your AOTD", "Fresh Indie", "notif-unread", "Mark all as read", "&lt;b&gt;nice")

	// Mentions link to the profile, and the rest stays escaped.
	_, body = alice.do("GET", "/posts/"+id, nil)
	mustContain(t, body, `<a href="/u/alice" class="mention">@alice</a> &lt;b&gt;nice&lt;/b&gt;`)

	// Opening one marks it read and goes to the comment.
	ns, _ := e.store.Notifications(2)
	if len(ns) != 0 {
		t.Errorf("bob has notifications: %+v", ns)
	}
	ns, _ = e.store.Notifications(1)
	mention := ns[1]
	if code, _ := bob.do("POST", fmt.Sprintf("/notifications/%d/open", mention.ID), nil); code != http.StatusNotFound {
		t.Errorf("open someone else's: %d", code)
	}
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/notifications/%d/open", e.srv.URL, mention.ID), nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	res, err := alice.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if loc := res.Header.Get("Location"); res.StatusCode != http.StatusSeeOther || loc != fmt.Sprintf("/posts/%s#comment-%d", id, *mention.CommentID) {
		t.Errorf("open: %d %q", res.StatusCode, loc)
	}
	_, body = alice.do("GET", "/", nil)
	mustContain(t, body, `<span class="badge">1</span>`)

	if code, _ := alice.do("POST", "/notifications/read", nil); code != http.StatusSeeOther {
		t.Errorf("mark all: %d", code)
	}
	_, body = alice.do("GET", "/notifications", nil)
	if strings.Contains(body, `class="badge"`) || strings.Contains(body, "notif-unread") || strings.Contains(body, "Mark all as read") {
		t.Error("still unread after mark all")
	}
}
