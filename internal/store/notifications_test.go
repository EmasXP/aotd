package store

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/EmasXP/aotd/internal/model"
)

// inbox describes userID's notifications, newest first, as "actor:type".
func inbox(t *testing.T, s *Store, userID uint) string {
	t.Helper()
	ns, err := s.Notifications(userID)
	if err != nil {
		t.Fatal(err)
	}
	var parts []string
	for _, n := range ns {
		parts = append(parts, n.Actor.Username+":"+n.Type)
	}
	return strings.Join(parts, " ")
}

func unread(t *testing.T, s *Store, userID uint) int64 {
	t.Helper()
	n, err := s.UnreadCount(userID)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMentions(t *testing.T) {
	for body, want := range map[string][]string{
		"hi @Alice and @bob_2!":       {"alice", "bob_2"},
		"@alice @ALICE":               {"alice"},
		"mail me at x@alice.com":      nil,
		"see example.com/@alice":      nil,
		"@al too short, @@alice":      nil,
		"(@carol) \"@dave\"":          {"carol", "dave"},
		"@" + strings.Repeat("a", 31): nil,
	} {
		if got := Mentions(body); !slices.Equal(got, want) {
			t.Errorf("Mentions(%q) = %v, want %v", body, got, want)
		}
	}
}

func TestCommentNotifications(t *testing.T) {
	s := newStore(t)
	alice, bob, carol, dave := mustUser(t, s, "alice"), mustUser(t, s, "bob"), mustUser(t, s, "carol"), mustUser(t, s, "dave")
	p := mustPost(t, s, alice, "Album")

	// Commenting on your own post notifies no one.
	s.AddComment(alice.ID, p.ID, nil, "my pick")
	if got := inbox(t, s, alice.ID); got != "" {
		t.Errorf("own comment notified: %q", got)
	}

	top, _ := s.AddComment(bob.ID, p.ID, nil, "great pick")
	if got := inbox(t, s, alice.ID); got != "bob:comment" {
		t.Errorf("owner inbox = %q", got)
	}

	// A reply notifies the owner, the thread starter and earlier repliers,
	// but not the replier.
	r1, _ := s.AddComment(carol.ID, p.ID, &top.ID, "agreed")
	s.AddComment(dave.ID, p.ID, &r1.ID, "me too")
	if got := inbox(t, s, alice.ID); got != "dave:comment carol:comment bob:comment" {
		t.Errorf("owner inbox = %q", got)
	}
	if got := inbox(t, s, bob.ID); got != "dave:reply carol:reply" {
		t.Errorf("thread starter inbox = %q", got)
	}
	if got := inbox(t, s, carol.ID); got != "dave:reply" {
		t.Errorf("replier inbox = %q", got)
	}
	if got := inbox(t, s, dave.ID); got != "" {
		t.Errorf("dave inbox = %q", got)
	}

	// The owner, once in the thread, gets a reply; a mention beats both.
	// Self-mentions and unknown names notify no one.
	s.AddComment(alice.ID, p.ID, &top.ID, "thanks all")
	s.AddComment(bob.ID, p.ID, &top.ID, "@alice @dave @bob @nobody")
	if got := inbox(t, s, alice.ID); !strings.HasPrefix(got, "bob:mention dave:comment") {
		t.Errorf("owner inbox = %q", got)
	}
	if got := inbox(t, s, dave.ID); got != "bob:mention alice:reply" {
		t.Errorf("dave inbox = %q", got)
	}
	if got := inbox(t, s, carol.ID); got != "bob:reply alice:reply dave:reply" {
		t.Errorf("carol inbox = %q", got)
	}
	if got := inbox(t, s, bob.ID); strings.Contains(got, "bob:") {
		t.Errorf("bob notified himself: %q", got)
	}

	// A mention on someone else's post notifies just the mentioned user.
	q := mustPost(t, s, carol, "Other")
	s.AddComment(bob.ID, q.ID, nil, "@dave you'd like this")
	if got := inbox(t, s, dave.ID); !strings.HasPrefix(got, "bob:mention bob:mention") {
		t.Errorf("dave inbox = %q", got)
	}
	ns, _ := s.Notifications(dave.ID)
	if n := ns[0]; n.Post.ID != q.ID || n.Post.Release.Title != "Other" || n.Snippet != "@dave you'd like this" {
		t.Errorf("view = %+v", n)
	}
}

func TestCheckInNotifications(t *testing.T) {
	s := newStore(t)
	alice, bob := mustUser(t, s, "alice"), mustUser(t, s, "bob")
	p := mustPost(t, s, alice, "Album")
	inbox(t, s, alice.ID) // prime the cache

	s.CheckIn(bob.ID, p.ID)
	s.CheckIn(bob.ID, p.ID) // idempotent: no second notification
	if got := inbox(t, s, alice.ID); got != "bob:checkin" {
		t.Errorf("inbox = %q", got)
	}
	s.UndoCheckIn(bob.ID, p.ID)
	if got := inbox(t, s, alice.ID); got != "" {
		t.Errorf("after undo = %q", got)
	}
	if n := unread(t, s, alice.ID); n != 0 {
		t.Errorf("unread after undo = %d", n)
	}
}

func TestNotificationsGoWithTheirComment(t *testing.T) {
	s := newStore(t)
	alice, bob, carol := mustUser(t, s, "alice"), mustUser(t, s, "bob"), mustUser(t, s, "carol")
	p := mustPost(t, s, alice, "Album")

	c, _ := s.AddComment(bob.ID, p.ID, nil, "hi @carol")
	s.AddComment(carol.ID, p.ID, nil, "hello")
	if unread(t, s, alice.ID) != 2 || unread(t, s, carol.ID) != 1 {
		t.Fatal("setup")
	}
	s.DeleteComment(bob.ID, c.ID)
	if got := inbox(t, s, alice.ID); got != "carol:comment" {
		t.Errorf("owner inbox = %q", got)
	}
	if n := unread(t, s, carol.ID); n != 0 {
		t.Errorf("carol unread = %d", n)
	}

	s.CheckIn(bob.ID, p.ID)
	s.AddComment(bob.ID, p.ID, nil, "@carol again")
	if unread(t, s, alice.ID) != 3 || unread(t, s, carol.ID) != 1 {
		t.Fatal("setup")
	}
	if err := s.DeletePost(alice.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	if a, c := unread(t, s, alice.ID), unread(t, s, carol.ID); a != 0 || c != 0 {
		t.Errorf("unread after post delete: alice=%d carol=%d", a, c)
	}
	if got := inbox(t, s, alice.ID) + inbox(t, s, carol.ID); got != "" {
		t.Errorf("after post delete = %q", got)
	}
	var n int64
	s.DB.Model(&model.Notification{}).Count(&n)
	if n != 0 {
		t.Errorf("%d notifications left", n)
	}
}

func TestReadNotifications(t *testing.T) {
	s := newStore(t)
	alice, bob, carol := mustUser(t, s, "alice"), mustUser(t, s, "bob"), mustUser(t, s, "carol")
	p := mustPost(t, s, alice, "Album")
	c, _ := s.AddComment(bob.ID, p.ID, nil, "one")
	s.CheckIn(bob.ID, p.ID)
	s.AddComment(carol.ID, p.ID, nil, "three")
	if n := unread(t, s, alice.ID); n != 3 {
		t.Fatalf("unread = %d", n)
	}
	ns, _ := s.Notifications(alice.ID)
	checkIn, comment := ns[1], ns[2]

	// Only the recipient can open one.
	if _, err := s.OpenNotification(bob.ID, comment.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("open someone else's: err = %v", err)
	}
	target, err := s.OpenNotification(alice.ID, comment.ID)
	if want := fmt.Sprintf("/posts/%d#comment-%d", p.ID, c.ID); err != nil || target != want {
		t.Errorf("open = %q, %v; want %q", target, err, want)
	}
	if target, _ := s.OpenNotification(alice.ID, checkIn.ID); target != fmt.Sprintf("/posts/%d", p.ID) {
		t.Errorf("open check-in = %q", target)
	}
	if n := unread(t, s, alice.ID); n != 1 {
		t.Errorf("unread after open = %d", n)
	}
	if ns, _ := s.Notifications(alice.ID); !ns[0].Unread() || ns[1].Unread() {
		t.Errorf("read state = %v %v", ns[0].ReadAt, ns[1].ReadAt)
	}

	// Marking someone else's as read does nothing.
	s.MarkNotificationRead(bob.ID, ns[0].ID)
	if n := unread(t, s, alice.ID); n != 1 {
		t.Errorf("unread after other's mark = %d", n)
	}
	s.MarkNotificationRead(alice.ID, ns[0].ID)
	if n := unread(t, s, alice.ID); n != 0 {
		t.Errorf("unread after mark = %d", n)
	}

	s.AddComment(bob.ID, p.ID, nil, "four")
	s.AddComment(carol.ID, p.ID, nil, "five")
	s.MarkAllNotificationsRead(alice.ID)
	if n := unread(t, s, alice.ID); n != 0 {
		t.Errorf("unread after mark all = %d", n)
	}
}

func TestPurgeNotifications(t *testing.T) {
	s := newStore(t)
	alice, bob := mustUser(t, s, "alice"), mustUser(t, s, "bob")
	p := mustPost(t, s, alice, "Album")

	at(t, s, "2026-01-01T12:00:00Z")
	s.AddComment(bob.ID, p.ID, nil, "old, unread")
	s.AddComment(bob.ID, p.ID, nil, "old, read")
	ns, _ := s.Notifications(alice.ID)
	s.MarkNotificationRead(alice.ID, ns[0].ID)
	at(t, s, "2026-02-15T12:00:00Z")
	s.AddComment(bob.ID, p.ID, nil, "newer, read")
	ns, _ = s.Notifications(alice.ID)
	s.MarkNotificationRead(alice.ID, ns[0].ID)

	snippets := func() string {
		ns, _ := s.Notifications(alice.ID)
		var out []string
		for _, n := range ns {
			out = append(out, n.Snippet)
		}
		return strings.Join(out, "; ")
	}
	at(t, s, "2026-03-15T12:00:00Z") // read 73 days, created 73 days ago
	s.PurgeNotifications()
	if got := snippets(); got != "newer, read; old, unread" {
		t.Errorf("after 73 days = %q", got)
	}
	at(t, s, "2026-04-02T12:00:00Z") // created 91 days ago
	s.PurgeNotifications()
	if got := snippets(); got != "newer, read" {
		t.Errorf("after 91 days = %q", got)
	}
	if n := unread(t, s, alice.ID); n != 0 {
		t.Errorf("unread = %d", n)
	}
	at(t, s, "2026-04-16T12:00:01Z") // read just over 60 days ago
	s.PurgeNotifications()
	if got := snippets(); got != "" {
		t.Errorf("after 60 days read = %q", got)
	}
}
