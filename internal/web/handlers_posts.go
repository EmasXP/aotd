package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/EmasXP/aotd/internal/musicbrainz"
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
func (s *Server) mbSearch(w http.ResponseWriter, r *http.Request) {
	album := strings.TrimSpace(r.URL.Query().Get("title"))
	artist := strings.TrimSpace(r.URL.Query().Get("artist"))
	data := map[string]any{}
	if len([]rune(album)) >= 2 || len([]rune(artist)) >= 2 {
		data["Searched"] = true
		albums, err := s.MB.Search(r.Context(), album, artist, 12)
		if err != nil {
			s.Log.Warn("musicbrainz search", "err", err)
			data["Error"] = "MusicBrainz isn't answering right now. You can post the album as typed below."
		}
		data["Albums"] = albums
	}
	s.partial(w, r, "mb_results", data)
}

func (s *Server) createPost(w http.ResponseWriter, r *http.Request) {
	me := currentUser(r)
	in := store.NewPost{Note: r.FormValue("note")}
	if r.FormValue("mode") == "manual" {
		in.Title = r.FormValue("title")
		in.Artist = r.FormValue("artist")
		if y := strings.TrimSpace(r.FormValue("year")); y != "" {
			n, err := strconv.Atoi(y)
			if err != nil {
				s.postError(w, r, "The year should be a number, like 1997.")
				return
			}
			in.Year = n
		}
	} else {
		mbid := r.FormValue("mbid")
		if mbid == "" {
			s.postError(w, r, "Search for your album and pick it from the results, or post it as typed.")
			return
		}
		// Re-fetch rather than trusting client-supplied metadata.
		a, err := s.MB.Lookup(r.Context(), mbid)
		if errors.Is(err, musicbrainz.ErrNotFound) {
			s.postError(w, r, "MusicBrainz doesn't know that album. Try searching again.")
			return
		} else if err != nil {
			s.Log.Warn("musicbrainz lookup", "err", err)
			s.postError(w, r, "MusicBrainz isn't answering right now. Try again, or enter the album manually.")
			return
		}
		in.MBID = &a.MBID
		in.Title, in.Artist, in.Year = a.Title, a.Artist, a.Year
		in.CoverURL = musicbrainz.CoverURL(a.MBID)
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
