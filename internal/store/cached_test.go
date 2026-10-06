package store

// Invalidation tests: each reads (priming the cache), writes, and reads
// again. newStore uses a real cache, so a missed invalidation is a stale
// read here.

import (
	"errors"
	"fmt"
	"testing"

	"github.com/EmasXP/aotd/internal/links"
)

// feedIDs(t)(s.Wall(...)) is the IDs of a feed page.
func feedIDs(t *testing.T) func(FeedPage, error) []uint {
	return func(page FeedPage, err error) []uint {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		var ids []uint
		for _, it := range page.Items {
			ids = append(ids, it.Post.ID)
		}
		return ids
	}
}

func TestCachedUser(t *testing.T) {
	s := newStore(t)
	alice := mustUser(t, s, "alice")
	s.UserByID(alice.ID)
	s.UserByUsername("alice")

	if err := s.UpdateProfile(alice.ID, "Alice A", "hi"); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.UserByUsername("Alice"); u.DisplayName != "Alice A" || u.Bio != "hi" {
		t.Errorf("after profile update: %+v", u)
	}
	s.SetAvatar(alice.ID, "a.jpg")
	if u, _ := s.UserByID(alice.ID); u.AvatarPath != "a.jpg" {
		t.Errorf("avatar = %q", u.AvatarPath)
	}
	s.SetPasswordHash(alice.ID, "new-hash")
	if u, _ := s.UserByID(alice.ID); u.PasswordHash != "new-hash" {
		t.Errorf("password hash = %q", u.PasswordHash)
	}
}

func TestCachedFollows(t *testing.T) {
	s := newStore(t)
	alice, bob := mustUser(t, s, "alice"), mustUser(t, s, "bob")
	p := mustPost(t, s, bob, "B")
	check := func(step string, following bool) {
		t.Helper()
		var n int64
		if following {
			n = 1
		}
		if s.IsFollowing(alice.ID, bob.ID) != following {
			t.Errorf("%s: IsFollowing = %v", step, !following)
		}
		if c := s.FollowCounts(bob.ID); c.Followers != n {
			t.Errorf("%s: bob's followers = %d", step, c.Followers)
		}
		if c := s.FollowCounts(alice.ID); c.Following != n {
			t.Errorf("%s: alice follows %d", step, c.Following)
		}
		if us, _ := s.Followers(bob.ID); int64(len(us)) != n {
			t.Errorf("%s: Followers = %v", step, us)
		}
		if us, _ := s.Following(alice.ID); int64(len(us)) != n {
			t.Errorf("%s: Following = %v", step, us)
		}
		ids := feedIDs(t)(s.Wall(alice.ID, ""))
		if (len(ids) == 1 && ids[0] == p.ID) != following {
			t.Errorf("%s: alice's wall = %v", step, ids)
		}
	}
	check("start", false)
	s.Follow(alice.ID, bob.ID)
	check("follow", true)
	s.Unfollow(alice.ID, bob.ID)
	check("unfollow", false)
}

func TestCachedFeedsOnPostAndDelete(t *testing.T) {
	s := newStore(t)
	alice, bob, carol := mustUser(t, s, "alice"), mustUser(t, s, "bob"), mustUser(t, s, "carol")
	s.Follow(bob.ID, alice.ID)
	g, _ := s.CreateGroup(carol.ID, "Crew", "")
	s.JoinGroup(alice.ID, g.ID)
	feeds := func() map[string][]uint {
		return map[string][]uint{
			"alice's wall": feedIDs(t)(s.Wall(alice.ID, "")),
			"bob's wall":   feedIDs(t)(s.Wall(bob.ID, "")),
			"group feed":   feedIDs(t)(s.GroupPosts(carol.ID, g.ID, "")),
			"profile":      feedIDs(t)(s.UserPosts(carol.ID, alice.ID, "")),
		}
	}
	for name, ids := range feeds() {
		if len(ids) != 0 {
			t.Errorf("start: %s = %v", name, ids)
		}
	}
	if _, err := s.PostOn(alice.ID, s.Today()); !errors.Is(err, ErrNotFound) {
		t.Errorf("PostOn before posting: %v", err)
	}

	p := mustPost(t, s, alice, "A")
	for name, ids := range feeds() {
		if len(ids) != 1 || ids[0] != p.ID {
			t.Errorf("after post: %s = %v", name, ids)
		}
	}
	if got, err := s.PostOn(alice.ID, s.Today()); err != nil || got.ID != p.ID {
		t.Errorf("PostOn after posting: %v, %v", got, err)
	}

	if err := s.UpdateNote(alice.ID, p.ID, "so good"); err != nil {
		t.Fatal(err)
	}
	if it, _ := s.Item(bob.ID, p.ID); it.Post.Note != "so good" {
		t.Errorf("note = %q", it.Post.Note)
	}

	if err := s.DeletePost(alice.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	for name, ids := range feeds() {
		if len(ids) != 0 {
			t.Errorf("after delete: %s = %v", name, ids)
		}
	}
	if _, err := s.PostByID(p.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("PostByID after delete: %v", err)
	}
}

func TestCachedPostDecorations(t *testing.T) {
	s := newStore(t)
	alice, bob := mustUser(t, s, "alice"), mustUser(t, s, "bob")
	p := mustPost(t, s, alice, "A")
	item := func() FeedItem {
		it, err := s.Item(bob.ID, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		return it
	}
	item()
	s.CheckedInUsers(p.ID)
	s.Comments(p.ID)

	s.CheckIn(bob.ID, p.ID)
	if it := item(); it.CheckIns != 1 || !it.CheckedIn {
		t.Errorf("after check-in: %d, %v", it.CheckIns, it.CheckedIn)
	}
	if us, _ := s.CheckedInUsers(p.ID); len(us) != 1 {
		t.Errorf("checked in = %v", us)
	}
	if ids := feedIDs(t)(s.UserCheckIns(alice.ID, bob.ID, "")); len(ids) != 1 {
		t.Errorf("bob's check-ins = %v", ids)
	}

	c, _ := s.AddComment(bob.ID, p.ID, nil, "nice")
	if it := item(); it.Comments != 1 {
		t.Errorf("comments = %d", it.Comments)
	}
	// A cached thread still shows its author's current name.
	s.UpdateProfile(bob.ID, "Bobby", "")
	if th, _ := s.Comments(p.ID); len(th) != 1 || th[0].Comment.User.DisplayName != "Bobby" {
		t.Errorf("threads = %+v", th)
	}
	s.DeleteComment(bob.ID, c.ID)
	if th, _ := s.Comments(p.ID); len(th) != 0 {
		t.Errorf("threads after delete = %+v", th)
	}
	// A post shows its author's current name too.
	s.UpdateProfile(alice.ID, "Alice A", "")
	if it := item(); it.Post.User.DisplayName != "Alice A" {
		t.Errorf("post author = %q", it.Post.User.DisplayName)
	}
}

func TestCachedReleases(t *testing.T) {
	s := newStore(t)
	alice, bob, carol := mustUser(t, s, "alice"), mustUser(t, s, "bob"), mustUser(t, s, "carol")
	pa, _ := s.CreatePost(alice.ID, NewPost{MBID: ptr(okc), Title: "OK Computer", Artist: "Radiohead"})
	shown := func() []links.Link {
		t.Helper()
		it, err := s.Item(alice.ID, pa.ID)
		if err != nil {
			t.Fatal(err)
		}
		var out []links.Link
		for _, l := range it.Links {
			out = append(out, l.Link())
		}
		return out
	}
	if got := shown(); len(got) != 0 {
		t.Errorf("start: %v", got)
	}
	remaster := links.Link{Source: links.Spotify, ID: "7dxKtc08dYeRVHt3p9CZJn"}
	s.AddLinks(pa.ReleaseID, []links.Link{spotifyOKC, remaster})
	if got := shown(); len(got) != 1 || got[0] != spotifyOKC {
		t.Errorf("after AddLinks: %v", got)
	}
	pb, _ := s.CreatePost(bob.ID, NewPost{Link: &remaster})
	if got := shown(); len(got) != 1 || got[0] != remaster {
		t.Errorf("after a post of the remaster: %v", got)
	}
	if err := s.DeletePost(bob.ID, pb.ID); err != nil {
		t.Fatal(err)
	}
	if got := shown(); len(got) != 1 || got[0] != spotifyOKC {
		t.Errorf("after deleting it: %v", got)
	}

	if r, _ := s.releaseByID(pa.ReleaseID); r.CheckedAt != nil {
		t.Fatalf("checked at %v", r.CheckedAt)
	}
	s.MarkChecked(pa.ReleaseID)
	if r, _ := s.releaseByID(pa.ReleaseID); r.CheckedAt == nil {
		t.Error("MarkChecked not seen")
	}

	// A manual post matched later moves to the existing MBID release.
	pc := mustPost(t, s, carol, "OK Computer")
	s.Item(alice.ID, pc.ID)
	r, err := s.MatchPost(carol.ID, pc.ID, NewPost{MBID: ptr(okc), Title: "OK Computer", Artist: "Radiohead"})
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != pa.ReleaseID {
		t.Fatalf("merged into %d, want %d", r.ID, pa.ReleaseID)
	}
	if p, _ := s.PostByID(pc.ID); p.ReleaseID != pa.ReleaseID || p.Release.MBID == nil {
		t.Errorf("matched post: release %d %+v", p.ReleaseID, p.Release)
	}
}

func TestCachedGroups(t *testing.T) {
	s := newStore(t)
	alice, bob := mustUser(t, s, "alice"), mustUser(t, s, "bob")
	names := func(gs []GroupSummary, err error) map[string]int64 {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]int64{}
		for _, g := range gs {
			m[g.Name] = g.Members
		}
		return m
	}
	if m := names(s.PopularGroups(10)); len(m) != 0 {
		t.Errorf("start: %v", m)
	}
	names(s.UserGroups(alice.ID))
	g, _ := s.CreateGroup(alice.ID, "Crew", "")
	if m := names(s.PopularGroups(10)); m["Crew"] != 1 {
		t.Errorf("popular after create: %v", m)
	}
	if m := names(s.UserGroups(alice.ID)); m["Crew"] != 1 {
		t.Errorf("alice's groups after create: %v", m)
	}

	names(s.UserGroups(bob.ID))
	s.GroupMembers(g.ID)
	pb := mustPost(t, s, bob, "B")
	if ids := feedIDs(t)(s.GroupPosts(alice.ID, g.ID, "")); len(ids) != 0 {
		t.Errorf("group feed before bob joins: %v", ids)
	}
	s.JoinGroup(bob.ID, g.ID)
	if !s.IsMember(bob.ID, g.ID) {
		t.Error("bob not a member after joining")
	}
	if us, _ := s.GroupMembers(g.ID); len(us) != 2 {
		t.Errorf("members = %v", us)
	}
	if m := names(s.UserGroups(bob.ID)); m["Crew"] != 2 {
		t.Errorf("bob's groups: %v", m)
	}
	if m := names(s.PopularGroups(10)); m["Crew"] != 2 {
		t.Errorf("popular after join: %v", m)
	}
	if ids := feedIDs(t)(s.GroupPosts(alice.ID, g.ID, "")); len(ids) != 1 || ids[0] != pb.ID {
		t.Errorf("group feed after bob joins: %v", ids)
	}
	s.LeaveGroup(bob.ID, g.ID)
	if s.IsMember(bob.ID, g.ID) {
		t.Error("bob still a member after leaving")
	}
	if m := names(s.UserGroups(bob.ID)); len(m) != 0 {
		t.Errorf("bob's groups after leaving: %v", m)
	}
	if ids := feedIDs(t)(s.GroupPosts(alice.ID, g.ID, "")); len(ids) != 0 {
		t.Errorf("group feed after bob leaves: %v", ids)
	}

	s.GroupBySlug("crew")
	if err := s.UpdateGroup(alice.ID, g.ID, "The Crew", "d"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GroupBySlug("crew"); got.Name != "The Crew" {
		t.Errorf("name after update = %q", got.Name)
	}
	if m := names(s.PopularGroups(10)); m["The Crew"] != 1 {
		t.Errorf("popular after update: %v", m)
	}

	s.JoinGroup(bob.ID, g.ID)
	names(s.UserGroups(bob.ID))
	if err := s.DeleteGroup(alice.ID, g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GroupBySlug("crew"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GroupBySlug after delete: %v", err)
	}
	if m := names(s.UserGroups(bob.ID)); len(m) != 0 {
		t.Errorf("bob's groups after delete: %v", m)
	}
	if m := names(s.PopularGroups(10)); len(m) != 0 {
		t.Errorf("popular after delete: %v", m)
	}
	// The slug is free for a new group.
	g2, _ := s.CreateGroup(bob.ID, "Crew", "")
	if got, err := s.GroupBySlug("crew"); err != nil || got.ID != g2.ID {
		t.Errorf("new group with the old slug: %v, %v", got, err)
	}
}

// A slug cached for a deleted group (e.g. read during its deletion) is
// noticed and looked up again.
func TestStaleSlugRecovers(t *testing.T) {
	s := newStore(t)
	alice := mustUser(t, s, "alice")
	g, _ := s.CreateGroup(alice.ID, "Crew", "")
	s.Cache.Set("slug:crew", []byte("999"), 0)
	if got, err := s.GroupBySlug("crew"); err != nil || got.ID != g.ID {
		t.Errorf("got %v, %v", got, err)
	}
	if b, ok, _ := s.Cache.Get("slug:crew"); ok && string(b) == "999" {
		t.Error("stale slug kept")
	}
}

func TestCachedDeleteKeepsPagesFull(t *testing.T) {
	s := newStore(t)
	alice, bob := mustUser(t, s, "alice"), mustUser(t, s, "bob")
	s.Follow(bob.ID, alice.ID)
	for d := 1; d <= FeedPageSize; d++ {
		at(t, s, fmt.Sprintf("2026-01-%02dT12:00:00Z", d))
		mustPost(t, s, alice, "Old")
	}
	at(t, s, "2026-01-25T12:00:00Z")
	// A release with an MBID outlives the post, so only the post's own
	// invalidation can hide it.
	p, err := s.CreatePost(alice.ID, NewPost{MBID: ptr(okc), Title: "OK Computer", Artist: "Radiohead"})
	if err != nil {
		t.Fatal(err)
	}
	page, _ := s.Wall(bob.ID, "")
	if len(page.Items) != FeedPageSize || page.Next == "" {
		t.Fatalf("before: %d items, next %q", len(page.Items), page.Next)
	}
	if err := s.DeletePost(alice.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PostByID(p.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("PostByID after delete: %v", err)
	}
	if page, _ = s.Wall(bob.ID, ""); len(page.Items) != FeedPageSize || page.Next != "" {
		t.Errorf("after delete: %d items, next %q", len(page.Items), page.Next)
	}
}

func TestCachedPopularOrder(t *testing.T) {
	s := newStore(t)
	alice, bob := mustUser(t, s, "alice"), mustUser(t, s, "bob")
	top := func() string {
		t.Helper()
		gs, err := s.PopularGroups(1)
		if err != nil {
			t.Fatal(err)
		}
		if len(gs) == 0 {
			return ""
		}
		return gs[0].Name
	}
	ga, _ := s.CreateGroup(alice.ID, "Alpha", "")
	gb, _ := s.CreateGroup(alice.ID, "Beta", "")
	if got := top(); got != "Alpha" {
		t.Errorf("tie: %q", got)
	}
	s.JoinGroup(bob.ID, gb.ID)
	if got := top(); got != "Beta" {
		t.Errorf("after join: %q", got)
	}
	s.LeaveGroup(bob.ID, gb.ID)
	if got := top(); got != "Alpha" {
		t.Errorf("after leave: %q", got)
	}
	s.UpdateGroup(alice.ID, ga.ID, "Zeta", "")
	if got := top(); got != "Beta" {
		t.Errorf("after rename: %q", got)
	}
	mustPost(t, s, alice, "A")
	s.GroupPosts(bob.ID, gb.ID, "")
	s.groupByID(gb.ID)
	s.DeleteGroup(alice.ID, gb.ID)
	if got := top(); got != "Zeta" {
		t.Errorf("after delete: %q", got)
	}
	if _, err := s.groupByID(gb.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted group: %v", err)
	}
	if ids := feedIDs(t)(s.GroupPosts(bob.ID, gb.ID, "")); len(ids) != 0 {
		t.Errorf("deleted group's feed: %v", ids)
	}
}
