package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/EmasXP/aotd/internal/links"
	"github.com/EmasXP/aotd/internal/musicbrainz"
	"github.com/EmasXP/aotd/internal/spotify"
	"github.com/EmasXP/aotd/internal/store"
)

func pathID(r *http.Request, name string) (uint, bool) {
	n, err := strconv.ParseUint(r.PathValue(name), 10, 64)
	return uint(n), err == nil && n > 0
}

func (s *Server) wall(w http.ResponseWriter, r *http.Request) {
	me := currentUser(r)
	page, err := s.Store.Wall(me.ID, store.Cursor(r.URL.Query().Get("before")))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if r.URL.Query().Get("fragment") == "1" {
		s.partial(w, r, "feed_items", map[string]any{"More": true, "Page": page, "MoreURL": "/?fragment=1&before="})
		return
	}
	data := map[string]any{"Title": "Wall", "Page": page, "MoreURL": "/?fragment=1&before="}
	s.addTodayBox(me.ID, data)
	if len(page.Items) < 3 {
		data["Suggestions"], _ = s.Store.SuggestUsers(me.ID, 6)
	}
	s.page(w, r, http.StatusOK, "wall", data)
}

// addTodayBox adds the viewer's post for today, if any, to data.
func (s *Server) addTodayBox(userID uint, data map[string]any) {
	if p, err := s.Store.PostOn(userID, s.Store.Today()); err == nil {
		data["MyToday"] = p
	}
}

// mbSearch returns album search results as radio options for the post form.
// A streaming link pasted as the album is resolved instead (see linkResults).
func (s *Server) mbSearch(w http.ResponseWriter, r *http.Request) {
	album := strings.TrimSpace(r.URL.Query().Get("title"))
	artist := strings.TrimSpace(r.URL.Query().Get("artist"))
	if l, ok := links.Parse(album); ok {
		s.partial(w, r, "mb_results", s.linkResults(r.Context(), l, artist))
		return
	}
	data := map[string]any{}
	if len([]rune(album)) >= 2 || len([]rune(artist)) >= 2 {
		data["Searched"] = true
		albums, err := s.MB.Search(r.Context(), album, artist, 12)
		if err != nil {
			s.Log.Warn("musicbrainz search", "err", err)
			data["Error"] = mbDown
		}
		data["Albums"] = albums
	}
	s.partial(w, r, "mb_results", data)
}

const mbDown = "MusicBrainz isn't answering right now. You can post the album as typed below."

// linkResults resolves a pasted link for the post form, in order: the
// release AOTD already has for it, the album MusicBrainz links it to, or the
// link's own title and cover with MusicBrainz candidates to confirm. A
// release without an MBID also gets candidates, so posting it can link it.
func (s *Server) linkResults(ctx context.Context, l links.Link, artist string) map[string]any {
	data := map[string]any{"Searched": true, "Link": l}
	candidates := func(title, artist string) {
		albums, err := s.MB.Search(ctx, title, artist, 6)
		if err != nil {
			s.Log.Warn("musicbrainz search", "err", err)
			data["Error"] = mbDown
		}
		data["Albums"], data["Confirm"] = albums, true
	}
	if rel, err := s.Store.ReleaseByLink(l); err == nil {
		data["Known"] = rel
		if rel.MBID == nil {
			candidates(rel.Title, rel.Artist)
		}
		return data
	} else if !errors.Is(err, store.ErrNotFound) {
		s.Log.Error("release by link", "err", err)
	}
	a, err := s.MB.LookupURL(ctx, l.URL())
	if err == nil {
		data["Albums"] = []musicbrainz.Album{a}
		return data
	} else if !errors.Is(err, musicbrainz.ErrNotFound) {
		s.Log.Warn("musicbrainz url lookup", "err", err)
	}
	p, err := s.preview(ctx, l)
	if err != nil {
		data["Error"] = "Couldn't read the album from that link. Type its title instead."
		return data
	}
	data["Preview"] = p
	candidates(p.Title, artist)
	return data
}

// preview reads an album's title and cover from its link, for links neither
// AOTD nor MusicBrainz knows.
func (s *Server) preview(ctx context.Context, l links.Link) (spotify.Album, error) {
	if l.Source != links.Spotify {
		return spotify.Album{}, errors.New("no preview for " + l.Source)
	}
	a, err := s.Spotify.Album(ctx, l.ID)
	if err != nil && !errors.Is(err, spotify.ErrNotFound) {
		s.Log.Warn("spotify album", "err", err)
	}
	return a, err
}

func (s *Server) createPost(w http.ResponseWriter, r *http.Request) {
	me := currentUser(r)
	in := store.NewPost{Note: r.FormValue("note")}
	// A link pasted as the album. If AOTD already knows it, it decides the
	// release and the album fields below only matter for linking it to an MBID.
	link, isLink := links.Parse(r.FormValue("title"))
	known := false
	if isLink {
		in.Link = &link
		_, err := s.Store.ReleaseByLink(link)
		known = err == nil
	}
	mbid := r.FormValue("mbid")
	switch {
	case r.FormValue("mode") == "manual":
		in.Title, in.Artist = r.FormValue("title"), r.FormValue("artist")
		if y := strings.TrimSpace(r.FormValue("year")); y != "" {
			n, err := strconv.Atoi(y)
			if err != nil {
				s.postError(w, r, "The year should be a number, like 1997.")
				return
			}
			in.Year = n
		}
		if isLink && !known {
			p, err := s.preview(r.Context(), link)
			if err != nil {
				s.postError(w, r, "Couldn't read the album from that link. Type its title instead.")
				return
			}
			in.Title, in.CoverURL = p.Title, p.CoverURL
		}
	case mbid != "" || (isLink && !known):
		// Re-fetch rather than trusting client-supplied metadata. A link
		// without a pick is fine if MusicBrainz knows it.
		var a musicbrainz.Album
		var err error
		if mbid != "" {
			a, err = s.MB.Lookup(r.Context(), mbid)
		} else {
			a, err = s.MB.LookupURL(r.Context(), link.URL())
		}
		if errors.Is(err, musicbrainz.ErrNotFound) {
			if mbid == "" {
				s.postError(w, r, "Pick the album from the results, or post it as typed.")
			} else {
				s.postError(w, r, "MusicBrainz doesn't know that album. Try searching again.")
			}
			return
		} else if err != nil {
			s.Log.Warn("musicbrainz lookup", "err", err)
			s.postError(w, r, "MusicBrainz isn't answering right now. Try again, or enter the album manually.")
			return
		}
		in.MBID = &a.MBID
		in.Title, in.Artist, in.Year = a.Title, a.Artist, a.Year
		in.CoverURL = musicbrainz.CoverURL(a.MBID)
	case !known:
		s.postError(w, r, "Search for your album and pick it from the results, or post it as typed.")
		return
	}
	if _, err := s.Store.CreatePost(me.ID, in); err != nil {
		var ve store.ValidationError
		if errors.As(err, &ve) || errors.Is(err, store.ErrAlreadyPosted) {
			s.postError(w, r, err.Error())
			return
		}
		s.serverError(w, r, err)
		return
	}
	s.Linker.Kick()
	redirect(w, r, "/")
}

// postError re-renders the wall with an error in the post form.
func (s *Server) postError(w http.ResponseWriter, r *http.Request, msg string) {
	me := currentUser(r)
	page, err := s.Store.Wall(me.ID, "")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	data := map[string]any{
		"Title": "Wall", "Page": page, "MoreURL": "/?fragment=1&before=", "PostError": msg,
		"Form": map[string]string{
			"note": r.FormValue("note"), "title": r.FormValue("title"),
			"artist": r.FormValue("artist"), "year": r.FormValue("year"), "mode": r.FormValue("mode"),
		},
	}
	s.addTodayBox(me.ID, data)
	s.page(w, r, http.StatusUnprocessableEntity, "wall", data)
}

func (s *Server) showPost(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.notFound(w, r)
		return
	}
	me := currentUser(r)
	item, err := s.Store.Item(me.ID, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	threads, err := s.Store.Comments(id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	checkedIn, _ := s.Store.CheckedInUsers(id)
	s.page(w, r, http.StatusOK, "post", map[string]any{
		"Title":     item.Post.Release.Title + " · " + item.Post.User.Name(),
		"Item":      item,
		"Threads":   threads,
		"CheckedIn": checkedIn,
		"Editable":  item.Post.UserID == me.ID && item.Post.PostDate == s.Store.Today(),
	})
}

func (s *Server) updateNote(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.notFound(w, r)
		return
	}
	if err := s.Store.UpdateNote(currentUser(r).ID, id, r.FormValue("note")); err != nil {
		s.fail(w, r, err)
		return
	}
	redirect(w, r, "/posts/"+r.PathValue("id"))
}

func (s *Server) deletePost(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.notFound(w, r)
		return
	}
	if err := s.Store.DeletePost(currentUser(r).ID, id); err != nil {
		s.fail(w, r, err)
		return
	}
	redirect(w, r, "/")
}

func (s *Server) checkIn(w http.ResponseWriter, r *http.Request)     { s.toggleCheckIn(w, r, true) }
func (s *Server) undoCheckIn(w http.ResponseWriter, r *http.Request) { s.toggleCheckIn(w, r, false) }

func (s *Server) toggleCheckIn(w http.ResponseWriter, r *http.Request, on bool) {
	id, ok := pathID(r, "id")
	if !ok {
		s.notFound(w, r)
		return
	}
	me := currentUser(r)
	var err error
	if on {
		err = s.Store.CheckIn(me.ID, id)
	} else {
		err = s.Store.UndoCheckIn(me.ID, id)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	item, err := s.Store.Item(me.ID, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.partial(w, r, "checkin", map[string]any{"Item": item})
}

func (s *Server) addComment(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.notFound(w, r)
		return
	}
	var parent *uint
	if p, err := strconv.ParseUint(r.FormValue("parent_id"), 10, 64); err == nil && p > 0 {
		pid := uint(p)
		parent = &pid
	}
	if _, err := s.Store.AddComment(currentUser(r).ID, id, parent, r.FormValue("body")); err != nil {
		var ve store.ValidationError
		if errors.As(err, &ve) {
			s.renderComments(w, r, id, err.Error())
			return
		}
		s.fail(w, r, err)
		return
	}
	s.renderComments(w, r, id, "")
}

func (s *Server) deleteComment(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		s.notFound(w, r)
		return
	}
	postID, err := s.Store.DeleteComment(currentUser(r).ID, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.renderComments(w, r, postID, "")
}

// renderComments responds with the comment section for postID. Without htmx
// it falls back to redirecting to the post page.
func (s *Server) renderComments(w http.ResponseWriter, r *http.Request, postID uint, errMsg string) {
	if !isFragment(r) {
		redirect(w, r, "/posts/"+strconv.FormatUint(uint64(postID), 10))
		return
	}
	threads, err := s.Store.Comments(postID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.partial(w, r, "comments", map[string]any{"PostID": postID, "Threads": threads, "Error": errMsg})
}
