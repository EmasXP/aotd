package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/EmasXP/aotd/internal/cache"
	"github.com/EmasXP/aotd/internal/db"
	"github.com/EmasXP/aotd/internal/model"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	g, err := db.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name()))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(g); err != nil {
		t.Fatal(err)
	}
	// A real cache, so a missed invalidation fails tests as a stale read.
	c, err := cache.OpenBolt(filepath.Join(t.TempDir(), "cache.db"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	s := New(g)
	s.Cache = c
	return s
}

func mustUser(t *testing.T, s *Store, name string) *model.User {
	t.Helper()
	u, err := s.CreateUser(name, name+"@example.com", name, "x")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func mustPost(t *testing.T, s *Store, u *model.User, title string) *model.Post {
	t.Helper()
	p, err := s.CreatePost(u.ID, NewPost{Title: title, Artist: "Artist"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func at(t *testing.T, s *Store, rfc3339 string) {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return ts }
}

func TestOnePostPerCETDay(t *testing.T) {
	s := newStore(t)
	alice := mustUser(t, s, "alice")

	at(t, s, "2026-01-15T22:30:00Z") // 23:30 CET on the 15th
	p := mustPost(t, s, alice, "First")
	if p.PostDate != "2026-01-15" {
		t.Fatalf("PostDate = %s", p.PostDate)
	}
	if _, err := s.CreatePost(alice.ID, NewPost{Title: "Second", Artist: "A"}); !errors.Is(err, ErrAlreadyPosted) {
		t.Fatalf("second post same day: err = %v", err)
	}

	at(t, s, "2026-01-15T23:05:00Z") // 00:05 CET on the 16th, still the 15th in UTC
	if p := mustPost(t, s, alice, "Next day"); p.PostDate != "2026-01-16" {
		t.Fatalf("PostDate = %s", p.PostDate)
	}
}

func TestUniqueIndexBacksDailyLimit(t *testing.T) {
	s := newStore(t)
	alice := mustUser(t, s, "alice")
	first := mustPost(t, s, alice, "First")
	// Bypass the pre-check, as a concurrent request would.
	err := s.DB.Omit("Release").Create(&model.Post{UserID: alice.ID, PostDate: s.Today(), ReleaseID: first.ReleaseID}).Error
	if err == nil {
		t.Fatal("database allowed two posts on one day")
	}
}

func TestDeleteFreesDayButOnlyToday(t *testing.T) {
	s := newStore(t)
	alice, bob := mustUser(t, s, "alice"), mustUser(t, s, "bob")
	at(t, s, "2026-03-01T10:00:00Z")
	old := mustPost(t, s, alice, "Old")
	at(t, s, "2026-03-02T10:00:00Z")
	if err := s.DeletePost(alice.ID, old.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("deleting yesterday's post: err = %v", err)
	}
	p := mustPost(t, s, alice, "Today")
	if err := s.DeletePost(bob.ID, p.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("deleting someone else's post: err = %v", err)
	}
	s.CheckIn(bob.ID, p.ID)
	s.AddComment(bob.ID, p.ID, nil, "nice")
	if err := s.DeletePost(alice.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	mustPost(t, s, alice, "Replacement")
}

func TestCheckInOnlyOthers(t *testing.T) {
	s := newStore(t)
	alice, bob := mustUser(t, s, "alice"), mustUser(t, s, "bob")
	p := mustPost(t, s, alice, "Album")
	if err := s.CheckIn(alice.ID, p.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("own check-in: err = %v", err)
	}
	for range 2 { // idempotent
		if err := s.CheckIn(bob.ID, p.ID); err != nil {
			t.Fatal(err)
		}
	}
	it, _ := s.Item(bob.ID, p.ID)
	if it.CheckIns != 1 || !it.CheckedIn {
		t.Errorf("item = %+v", it)
	}
	page, _ := s.UserCheckIns(alice.ID, bob.ID, "")
	if len(page.Items) != 1 || page.Items[0].Post.ID != p.ID {
		t.Errorf("bob's check-ins = %+v", page.Items)
	}
	s.UndoCheckIn(bob.ID, p.ID)
	if it, _ := s.Item(bob.ID, p.ID); it.CheckIns != 0 || it.CheckedIn {
		t.Errorf("after undo = %+v", it)
	}
}

func TestCheckInIsPerPostNotAlbum(t *testing.T) {
	s := newStore(t)
	alice, bob, carol := mustUser(t, s, "alice"), mustUser(t, s, "bob"), mustUser(t, s, "carol")
	mbid := "b1392450-e666-3926-a536-22c65f834433"
	pa, _ := s.CreatePost(alice.ID, NewPost{MBID: &mbid, Title: "OK Computer", Artist: "Radiohead"})
	pb, _ := s.CreatePost(bob.ID, NewPost{MBID: &mbid, Title: "OK Computer", Artist: "Radiohead"})
	s.CheckIn(carol.ID, pa.ID)
	a, _ := s.Item(carol.ID, pa.ID)
	b, _ := s.Item(carol.ID, pb.ID)
	if a.CheckIns != 1 || b.CheckIns != 0 {
		t.Errorf("counts: a=%d b=%d", a.CheckIns, b.CheckIns)
	}
}

func TestCommentsOneLevel(t *testing.T) {
	s := newStore(t)
	alice, bob := mustUser(t, s, "alice"), mustUser(t, s, "bob")
	p := mustPost(t, s, alice, "Album")
	top, _ := s.AddComment(bob.ID, p.ID, nil, "great pick")
	r1, _ := s.AddComment(alice.ID, p.ID, &top.ID, "thanks")
	r2, err := s.AddComment(bob.ID, p.ID, &r1.ID, "reply to reply")
	if err != nil {
		t.Fatal(err)
	}
	if r2.ParentID == nil || *r2.ParentID != top.ID {
		t.Errorf("reply-to-reply parent = %v, want %d", r2.ParentID, top.ID)
	}
	threads, _ := s.Comments(p.ID)
	if len(threads) != 1 || len(threads[0].Replies) != 2 {
		t.Fatalf("threads = %+v", threads)
	}

	// Parent from another post is rejected.
	other := mustPost(t, s, bob, "Other")
	if _, err := s.AddComment(bob.ID, other.ID, &top.ID, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-post parent: err = %v", err)
	}

	// Only the author can delete; a deleted parent with replies stays as a placeholder.
	if _, err := s.DeleteComment(alice.ID, top.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("delete by non-author: err = %v", err)
	}
	s.DeleteComment(bob.ID, top.ID)
	threads, _ = s.Comments(p.ID)
	if len(threads) != 1 || !threads[0].Deleted || threads[0].Comment.Body != "" {
		t.Errorf("after delete = %+v", threads)
	}
	s.DeleteComment(alice.ID, r1.ID)
	s.DeleteComment(bob.ID, r2.ID)
	if threads, _ = s.Comments(p.ID); len(threads) != 0 {
		t.Errorf("fully deleted thread still shown: %+v", threads)
	}
}

func TestWallScopeAndPagination(t *testing.T) {
	s := newStore(t)
	alice, bob, carol := mustUser(t, s, "alice"), mustUser(t, s, "bob"), mustUser(t, s, "carol")
	s.Follow(alice.ID, bob.ID)
	if err := s.Follow(alice.ID, alice.ID); err == nil {
		t.Error("self-follow allowed")
	}
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for i := range 25 {
		ts := start.AddDate(0, 0, i)
		s.Now = func() time.Time { return ts }
		mustPost(t, s, alice, fmt.Sprintf("A%d", i))
		mustPost(t, s, bob, fmt.Sprintf("B%d", i))
		mustPost(t, s, carol, fmt.Sprintf("C%d", i))
	}
	seen := map[uint]bool{}
	var cur Cursor
	pages := 0
	for {
		page, err := s.Wall(alice.ID, cur)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, it := range page.Items {
			if it.Post.UserID == carol.ID {
				t.Fatal("unfollowed user's post on wall")
			}
			if seen[it.Post.ID] {
				t.Fatal("duplicate across pages")
			}
			seen[it.Post.ID] = true
		}
		if page.Next == "" {
			break
		}
		cur = page.Next
	}
	if len(seen) != 50 || pages != 3 {
		t.Errorf("saw %d posts over %d pages", len(seen), pages)
	}
	first, _ := s.Wall(alice.ID, "")
	if first.Items[0].Post.PostDate != "2026-01-25" {
		t.Errorf("newest first: got %s", first.Items[0].Post.PostDate)
	}
}

func TestGroups(t *testing.T) {
	s := newStore(t)
	alice, bob, carol := mustUser(t, s, "alice"), mustUser(t, s, "bob"), mustUser(t, s, "carol")
	g, err := s.CreateGroup(alice.ID, "Masters of FTT", "")
	if err != nil {
		t.Fatal(err)
	}
	if g.Slug != "masters-of-ftt" {
		t.Errorf("slug = %s", g.Slug)
	}
	g2, _ := s.CreateGroup(bob.ID, "Masters of FTT!", "")
	if g2.Slug != "masters-of-ftt-2" {
		t.Errorf("second slug = %s", g2.Slug)
	}
	s.JoinGroup(bob.ID, g.ID)
	mustPost(t, s, alice, "A")
	mustPost(t, s, bob, "B")
	mustPost(t, s, carol, "C")
	page, _ := s.GroupPosts(carol.ID, g.ID, "")
	if len(page.Items) != 2 {
		t.Errorf("group feed has %d items, want 2", len(page.Items))
	}
	s.LeaveGroup(alice.ID, g.ID) // owner can't leave
	if !s.IsMember(alice.ID, g.ID) {
		t.Error("owner left group")
	}
	if err := s.UpdateGroup(bob.ID, g.ID, "Hijack", ""); !errors.Is(err, ErrForbidden) {
		t.Errorf("non-owner update: err = %v", err)
	}
	if gs, _ := s.UserGroups(bob.ID); len(gs) != 2 || gs[0].Members == 0 {
		t.Errorf("bob's groups = %+v", gs)
	}
}

func TestSearchEscapesWildcards(t *testing.T) {
	s := newStore(t)
	mustUser(t, s, "under_score")
	mustUser(t, s, "underxscore")
	us, _ := s.SearchUsers("r_s", 20)
	if len(us) != 1 || us[0].Username != "under_score" {
		t.Errorf("search r_s = %+v", us)
	}
	if us, _ := s.SearchUsers("%", 20); len(us) != 0 {
		t.Errorf("search %% matched %d users", len(us))
	}
	s.CreateGroup(1, "Jazz 100%", "")
	s.CreateGroup(1, "Jazz 1000", "")
	if gs, _ := s.SearchGroups("100%", 20); len(gs) != 1 {
		t.Errorf("group search 100%% = %d", len(gs))
	}
	if us, _ := s.SearchUsers("UNDER", 20); len(us) != 2 {
		t.Errorf("case-insensitive search = %d", len(us))
	}
}

func TestCreateUserDuplicates(t *testing.T) {
	s := newStore(t)
	mustUser(t, s, "alice")
	if _, err := s.CreateUser("alice", "other@example.com", "A", "x"); !errors.Is(err, ErrUsernameTaken) {
		t.Errorf("dup username: %v", err)
	}
	if _, err := s.CreateUser("alice2", "alice@example.com", "A", "x"); !errors.Is(err, ErrEmailTaken) {
		t.Errorf("dup email: %v", err)
	}
	if _, _, _, err := NormalizeSignup("Al", "a@b.se", ""); err == nil {
		t.Error("short username accepted")
	}
	u, e, d, err := NormalizeSignup(" Alice_1 ", "ALICE@Example.com", "")
	if err != nil || u != "alice_1" || e != "alice@example.com" || d != "alice_1" {
		t.Errorf("normalize = %q %q %q %v", u, e, d, err)
	}
}

func TestSetAvatarReturnsPrevious(t *testing.T) {
	s := newStore(t)
	u := mustUser(t, s, "alice")
	if old, _ := s.SetAvatar(u.ID, "a.jpg"); old != "" {
		t.Errorf("first old = %q", old)
	}
	if old, _ := s.SetAvatar(u.ID, "b.jpg"); old != "a.jpg" {
		t.Errorf("second old = %q, want a.jpg", old)
	}
}

func TestPostsShareReleaseByMBID(t *testing.T) {
	s := newStore(t)
	alice, bob, carol := mustUser(t, s, "alice"), mustUser(t, s, "bob"), mustUser(t, s, "carol")
	mbid := "b1392450-e666-3926-a536-22c65f834433"
	pa, err := s.CreatePost(alice.ID, NewPost{MBID: &mbid, Title: "OK Computr", Artist: "Radiohead"})
	if err != nil {
		t.Fatal(err)
	}
	pb, err := s.CreatePost(bob.ID, NewPost{MBID: &mbid, Title: "OK Computer", Artist: "Radiohead", Year: 1997})
	if err != nil {
		t.Fatal(err)
	}
	if pa.ReleaseID != pb.ReleaseID || pa.ReleaseID == 0 {
		t.Fatalf("release ids %d, %d", pa.ReleaseID, pb.ReleaseID)
	}
	if pb.Release.ID != pb.ReleaseID {
		t.Errorf("returned post's release = %+v", pb.Release)
	}
	// The latest MusicBrainz metadata wins.
	if p, _ := s.PostByID(pa.ID); p.Release.Title != "OK Computer" || p.Release.Year != 1997 {
		t.Errorf("release = %+v", p.Release)
	}
	// Manual entries never share, even with the same title.
	m1, _ := s.CreatePost(carol.ID, NewPost{Title: "OK Computer", Artist: "Radiohead"})
	if m1.ReleaseID == pa.ReleaseID {
		t.Error("manual entry joined the MusicBrainz release")
	}
}

func TestDeletePostRemovesOrphanedManualRelease(t *testing.T) {
	s := newStore(t)
	alice, bob := mustUser(t, s, "alice"), mustUser(t, s, "bob")
	mbid := "b1392450-e666-3926-a536-22c65f834433"
	manual := mustPost(t, s, alice, "Demo")
	if err := s.DeletePost(alice.ID, manual.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.First(&model.Release{}, manual.ReleaseID).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Errorf("manual release kept: %v", err)
	}
	p, _ := s.CreatePost(bob.ID, NewPost{MBID: &mbid, Title: "OK Computer", Artist: "Radiohead"})
	if err := s.DeletePost(bob.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.First(&model.Release{}, p.ReleaseID).Error; err != nil {
		t.Errorf("MusicBrainz release deleted: %v", err)
	}
}

func TestActivityCounts(t *testing.T) {
	s := newStore(t)
	alice, bob := mustUser(t, s, "alice"), mustUser(t, s, "bob")
	check := func(step string, u *model.User, want ActivityCounts) {
		t.Helper()
		if got := s.ActivityCounts(u.ID); got != want {
			t.Errorf("%s: %s = %+v, want %+v", step, u.Username, got, want)
		}
	}
	// Each check also primes the cache, so a missed invalidation shows up.
	check("start", alice, ActivityCounts{})
	check("start", bob, ActivityCounts{})

	p := mustPost(t, s, alice, "Album")
	check("post", alice, ActivityCounts{Posts: 1})
	s.CheckIn(bob.ID, p.ID)
	check("check-in", bob, ActivityCounts{CheckIns: 1})
	top, _ := s.AddComment(bob.ID, p.ID, nil, "nice")
	s.AddComment(bob.ID, p.ID, &top.ID, "really")
	s.AddComment(alice.ID, p.ID, nil, "thanks")
	check("comments", bob, ActivityCounts{CheckIns: 1, Comments: 2})
	check("comments", alice, ActivityCounts{Posts: 1, Comments: 1})
	s.DeleteComment(bob.ID, top.ID)
	check("delete comment", bob, ActivityCounts{CheckIns: 1, Comments: 1})
	s.UndoCheckIn(bob.ID, p.ID)
	check("undo check-in", bob, ActivityCounts{Comments: 1})
	s.CheckIn(bob.ID, p.ID)
	check("check-in again", bob, ActivityCounts{CheckIns: 1, Comments: 1})

	// Deleting the post takes others' check-ins and comments with it.
	if err := s.DeletePost(alice.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	check("delete post", alice, ActivityCounts{})
	check("delete post", bob, ActivityCounts{})
}
