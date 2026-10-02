package store

import (
	"errors"
	"testing"

	"gorm.io/gorm"

	"github.com/EmasXP/aotd/internal/links"
	"github.com/EmasXP/aotd/internal/model"
)

const okc = "b1392450-e666-3926-a536-22c65f834433"

var spotifyOKC = links.Link{Source: links.Spotify, ID: "6dVIqQ8qmQ5GBnJ9shOYGE"}

func ptr[T any](v T) *T { return &v }

func TestLinkedPostSharesLinkWithMBIDPost(t *testing.T) {
	s := newStore(t)
	alice, bob := mustUser(t, s, "alice"), mustUser(t, s, "bob")
	pa, err := s.CreatePost(alice.ID, NewPost{MBID: ptr(okc), Title: "OK Computer", Artist: "Radiohead", Link: &spotifyOKC})
	if err != nil {
		t.Fatal(err)
	}
	if pa.LinkID == nil {
		t.Fatal("post has no link")
	}
	pb, _ := s.CreatePost(bob.ID, NewPost{MBID: ptr(okc), Title: "OK Computer", Artist: "Radiohead"})
	it, _ := s.Item(bob.ID, pb.ID)
	if len(it.Links) != 1 || it.Links[0].Link() != spotifyOKC {
		t.Errorf("Bob's post links = %+v", it.Links)
	}
}

func TestKnownLinkDecidesRelease(t *testing.T) {
	s := newStore(t)
	alice, bob := mustUser(t, s, "alice"), mustUser(t, s, "bob")
	pa, err := s.CreatePost(alice.ID, NewPost{Title: "Fresh Indie", Artist: "Band", CoverURL: "spotify-cover", Link: &spotifyOKC})
	if err != nil {
		t.Fatal(err)
	}
	if pa.Release.MBID != nil {
		t.Fatal("manual release got an MBID")
	}
	// Bob's typed title is ignored; the link says which album it is.
	pb, err := s.CreatePost(bob.ID, NewPost{Title: "", Artist: "", Link: &spotifyOKC})
	if err != nil {
		t.Fatal(err)
	}
	if pb.ReleaseID != pa.ReleaseID || pb.Release.Title != "Fresh Indie" || *pb.LinkID != *pa.LinkID {
		t.Errorf("Bob's post = %+v", pb)
	}
	if r, err := s.ReleaseByLink(spotifyOKC); err != nil || r.ID != pa.ReleaseID {
		t.Errorf("ReleaseByLink = %+v, %v", r, err)
	}
	if _, err := s.ReleaseByLink(links.Link{Source: links.Deezer, ID: "1"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown link err = %v", err)
	}
}

func TestLinkGivesReleaseItsMBID(t *testing.T) {
	s := newStore(t)
	alice, bob := mustUser(t, s, "alice"), mustUser(t, s, "bob")
	pa, _ := s.CreatePost(alice.ID, NewPost{Title: "OK Computr", Artist: "Radiohead", CoverURL: "spotify-cover", Link: &spotifyOKC})
	// Later, Bob posts the same link and confirms the MusicBrainz match.
	pb, err := s.CreatePost(bob.ID, NewPost{MBID: ptr(okc), Title: "OK Computer", Artist: "Radiohead", Year: 1997, CoverURL: "caa-cover", Link: &spotifyOKC})
	if err != nil {
		t.Fatal(err)
	}
	if pb.ReleaseID != pa.ReleaseID {
		t.Fatal("a second release was created")
	}
	r := pb.Release
	if r.MBID == nil || *r.MBID != okc || r.Title != "OK Computer" || r.Year != 1997 || r.CoverURL != "spotify-cover" {
		t.Errorf("release = %+v; want MusicBrainz metadata and the existing cover", r)
	}
}

func TestLinkMergesIntoExistingMBIDRelease(t *testing.T) {
	s := newStore(t)
	alice, bob, carol := mustUser(t, s, "alice"), mustUser(t, s, "bob"), mustUser(t, s, "carol")
	// Alice posts from Spotify without a match; Bob posts the same album from MusicBrainz.
	pa, _ := s.CreatePost(alice.ID, NewPost{Title: "OK Computer", Artist: "Radiohead", Link: &spotifyOKC})
	pb, _ := s.CreatePost(bob.ID, NewPost{MBID: ptr(okc), Title: "OK Computer", Artist: "Radiohead"})
	if pa.ReleaseID == pb.ReleaseID {
		t.Fatal("expected two releases before the match")
	}
	// Carol posts Alice's link and confirms the match: the releases merge.
	pc, err := s.CreatePost(carol.ID, NewPost{MBID: ptr(okc), Title: "OK Computer", Artist: "Radiohead", Link: &spotifyOKC})
	if err != nil {
		t.Fatal(err)
	}
	if pc.ReleaseID != pb.ReleaseID {
		t.Errorf("Carol's post is on release %d, want %d", pc.ReleaseID, pb.ReleaseID)
	}
	if p, _ := s.PostByID(pa.ID); p.ReleaseID != pb.ReleaseID {
		t.Errorf("Alice's post wasn't moved: release %d", p.ReleaseID)
	}
	if r, _ := s.ReleaseByLink(spotifyOKC); r.ID != pb.ReleaseID {
		t.Errorf("link is on release %d", r.ID)
	}
	if err := s.DB.First(&model.Release{}, pa.ReleaseID).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Errorf("merged release still exists: %v", err)
	}
}

func TestMostPostedLinkIsShown(t *testing.T) {
	s := newStore(t)
	alice, bob, carol := mustUser(t, s, "alice"), mustUser(t, s, "bob"), mustUser(t, s, "carol")
	pa, _ := s.CreatePost(alice.ID, NewPost{MBID: ptr(okc), Title: "OK Computer", Artist: "Radiohead"})
	remaster := links.Link{Source: links.Spotify, ID: "7dxKtc08dYeRVHt3p9CZJn"}
	if err := s.AddLinks(pa.ReleaseID, []links.Link{spotifyOKC, remaster, {Source: links.Deezer, ID: "14879699"}}); err != nil {
		t.Fatal(err)
	}
	shown := func() []links.Link {
		it, _ := s.Item(alice.ID, pa.ID)
		var out []links.Link
		for _, l := range it.Links {
			out = append(out, l.Link())
		}
		return out
	}
	if got := shown(); len(got) != 2 || got[0] != spotifyOKC || got[1].Source != links.Deezer {
		t.Errorf("before any linked posts: %v", got)
	}
	s.CreatePost(bob.ID, NewPost{Link: &remaster})
	s.CreatePost(carol.ID, NewPost{Link: &remaster})
	if got := shown(); len(got) != 2 || got[0] != remaster {
		t.Errorf("after two posts of the remaster: %v", got)
	}
}
