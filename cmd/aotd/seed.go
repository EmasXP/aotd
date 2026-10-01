package main

import (
	"errors"
	"log/slog"
	"time"

	"github.com/EmasXP/aotd/internal/auth"
	"github.com/EmasXP/aotd/internal/model"
	"github.com/EmasXP/aotd/internal/musicbrainz"
	"github.com/EmasXP/aotd/internal/store"
	"github.com/EmasXP/aotd/internal/web"
)

const demoPassword = "spin the black circle"

type demoAlbum struct {
	mbid, title, artist string
	year                int
}

var demoAlbums = []demoAlbum{
	{"8e8a594f-2175-38c7-a871-abb68ec363e7", "Kind of Blue", "Miles Davis", 1959},
	{"b1392450-e666-3926-a536-22c65f834433", "OK Computer", "Radiohead", 1997},
	{"48117b90-a16e-34ca-a514-19c702df1158", "Discovery", "Daft Punk", 2001},
	{"d9103c72-3807-4378-9ce7-b6f3e8fdd547", "To Pimp a Butterfly", "Kendrick Lamar", 2015},
	{"810272e0-aef1-3d85-b2d3-e512e87fc38c", "Homogenic", "Björk", 1997},
	{"9162580e-5df4-32de-80cc-f45a8d8a9b1d", "Abbey Road", "The Beatles", 1969},
	{"", "Live at the Office Christmas Party", "The FTT House Band", 2025}, // manual entry
}

// seed adds demo data. It is idempotent: if the demo users exist it does nothing.
func seed(st *store.Store, srv *web.Server) error {
	if _, err := st.UserByUsername("alice"); err == nil {
		slog.Info("seed: demo data already present")
		return nil
	}
	hash, err := auth.HashPassword(demoPassword, srv.Params)
	if err != nil {
		return err
	}
	people := []struct{ username, name, bio string }{
		{"alice", "Alice Andersson", "Jazz in the morning, techno at night."},
		{"bob", "Bob Berg", "Mostly 90s. Sometimes 70s. Never ska."},
		{"carol", "Carol Chen", "Vinyl hoarder. Ask me about Björk."},
	}
	var users []*model.User
	for _, p := range people {
		u, err := st.CreateUser(p.username, p.username+"@example.com", p.name, hash)
		if err != nil {
			return err
		}
		if err := st.UpdateProfile(u.ID, p.name, p.bio); err != nil {
			return err
		}
		users = append(users, u)
	}
	for _, a := range users {
		for _, b := range users {
			if a.ID != b.ID {
				st.Follow(a.ID, b.ID)
			}
		}
	}

	notes := []string{
		"Perfect rainy-day record.",
		"",
		"Put this on with good headphones. Trust me.",
		"Still sounds like the future.",
		"",
	}
	realNow := st.Now
	defer func() { st.Now = realNow }()
	var posts []*model.Post
	i := 0
	for daysAgo := 6; daysAgo >= 1; daysAgo-- {
		ts := time.Now().AddDate(0, 0, -daysAgo)
		st.Now = func() time.Time { return ts }
		for _, u := range users {
			if (daysAgo+int(u.ID))%3 == 0 { // nobody posts every day
				continue
			}
			a := demoAlbums[i%len(demoAlbums)]
			i++
			in := store.NewPost{Title: a.title, Artist: a.artist, Year: a.year, Note: notes[i%len(notes)]}
			if a.mbid != "" {
				in.MBID = &a.mbid
				in.CoverURL = musicbrainz.CoverURL(a.mbid)
			}
			p, err := st.CreatePost(u.ID, in)
			if err != nil && !errors.Is(err, store.ErrAlreadyPosted) {
				return err
			}
			if p != nil {
				posts = append(posts, p)
			}
		}
	}
	st.Now = realNow

	for n, p := range posts {
		for _, u := range users {
			if u.ID != p.UserID && (n+int(u.ID))%2 == 0 {
				st.CheckIn(u.ID, p.ID)
			}
		}
	}
	if len(posts) > 0 {
		p := posts[len(posts)-1]
		var other, third *model.User
		for _, u := range users {
			if u.ID != p.UserID {
				if other == nil {
					other = u
				} else {
					third = u
				}
			}
		}
		c, _ := st.AddComment(other.ID, p.ID, nil, "Great pick! Haven't heard this in ages.")
		st.AddComment(p.UserID, p.ID, &c.ID, "Right? The second half is the best part.")
		st.AddComment(third.ID, p.ID, nil, "Adding it to my list for tomorrow.")
	}

	g, err := st.CreateGroup(users[0].ID, "Masters of FTT", "The original AOTD crew. Years of albums, one a day.")
	if err != nil {
		return err
	}
	for _, u := range users[1:] {
		st.JoinGroup(u.ID, g.ID)
	}
	slog.Info("seed: created demo users alice, bob, carol", "password", demoPassword)
	return nil
}
