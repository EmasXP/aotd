package web

import (
	"errors"
	"net/http"
	"path/filepath"
	"regexp"

	"github.com/EmasXP/aotd/internal/auth"
	"github.com/EmasXP/aotd/internal/avatar"
	"github.com/EmasXP/aotd/internal/model"
	"github.com/EmasXP/aotd/internal/store"
)

// userFromPath loads the user named in the URL, responding 404 if missing.
func (s *Server) userFromPath(w http.ResponseWriter, r *http.Request) (*model.User, bool) {
	u, err := s.Store.UserByUsername(r.PathValue("username"))
	if err != nil {
		s.fail(w, r, err)
		return nil, false
	}
	return u, true
}

func (s *Server) profileHeader(me, u *model.User) map[string]any {
	groups, _ := s.Store.UserGroups(u.ID)
	return map[string]any{
		"Title":     u.Name(),
		"User":      u,
		"Counts":    s.Store.FollowCounts(u.ID),
		"Following": me.ID != u.ID && s.Store.IsFollowing(me.ID, u.ID),
		"Groups":    groups,
	}
}

func (s *Server) profile(w http.ResponseWriter, r *http.Request) {
	u, ok := s.userFromPath(w, r)
	if !ok {
		return
	}
	me := currentUser(r)
	tab := r.URL.Query().Get("tab")
	if tab != "checkins" {
		tab = "aotds"
	}
	before := store.Cursor(r.URL.Query().Get("before"))
	var page store.FeedPage
	var err error
	if tab == "checkins" {
		page, err = s.Store.UserCheckIns(me.ID, u.ID, before)
	} else {
		page, err = s.Store.UserPosts(me.ID, u.ID, before)
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	moreURL := "/u/" + u.Username + "?tab=" + tab + "&fragment=1&before="
	if r.URL.Query().Get("fragment") == "1" {
		s.partial(w, r, "feed_items", map[string]any{"More": true, "Page": page, "MoreURL": moreURL})
		return
	}
	data := s.profileHeader(me, u)
	data["Tab"], data["Page"], data["MoreURL"] = tab, page, moreURL
	s.page(w, r, http.StatusOK, "profile", data)
}

func (s *Server) followers(w http.ResponseWriter, r *http.Request) {
	s.followList(w, r, "followers")
}

func (s *Server) following(w http.ResponseWriter, r *http.Request) {
	s.followList(w, r, "following")
}

func (s *Server) followList(w http.ResponseWriter, r *http.Request, which string) {
	u, ok := s.userFromPath(w, r)
	if !ok {
		return
	}
	var users []model.User
	var err error
	if which == "followers" {
		users, err = s.Store.Followers(u.ID)
	} else {
		users, err = s.Store.Following(u.ID)
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	data := s.profileHeader(currentUser(r), u)
	data["Tab"], data["Users"] = which, users
	s.page(w, r, http.StatusOK, "profile", data)
}

func (s *Server) follow(w http.ResponseWriter, r *http.Request)   { s.setFollow(w, r, true) }
func (s *Server) unfollow(w http.ResponseWriter, r *http.Request) { s.setFollow(w, r, false) }

func (s *Server) setFollow(w http.ResponseWriter, r *http.Request, on bool) {
	u, ok := s.userFromPath(w, r)
	if !ok {
		return
	}
	me := currentUser(r)
	var err error
	if on {
		err = s.Store.Follow(me.ID, u.ID)
	} else {
		err = s.Store.Unfollow(me.ID, u.ID)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.partial(w, r, "follow_button", map[string]any{"User": u, "Following": on})
}

// --- settings ---

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	s.page(w, r, http.StatusOK, "settings", map[string]any{"Title": "Settings", "Saved": r.URL.Query().Get("saved")})
}

func (s *Server) settingsError(w http.ResponseWriter, r *http.Request, section, msg string) {
	s.page(w, r, http.StatusUnprocessableEntity, "settings", map[string]any{
		"Title": "Settings", "Errors": map[string]string{section: msg},
	})
}

func (s *Server) saveProfile(w http.ResponseWriter, r *http.Request) {
	err := s.Store.UpdateProfile(currentUser(r).ID, r.FormValue("display_name"), r.FormValue("bio"))
	var ve store.ValidationError
	if errors.As(err, &ve) {
		s.settingsError(w, r, "profile", err.Error())
		return
	} else if err != nil {
		s.serverError(w, r, err)
		return
	}
	redirect(w, r, "/settings?saved=profile")
}

func (s *Server) saveAvatar(w http.ResponseWriter, r *http.Request) {
	f, _, err := r.FormFile("avatar")
	if err != nil {
		s.settingsError(w, r, "avatar", avatar.ErrInvalid.Error())
		return
	}
	defer f.Close()
	jpg, err := avatar.Process(f)
	if err != nil {
		s.settingsError(w, r, "avatar", avatar.ErrInvalid.Error())
		return
	}
	name, err := avatar.Save(s.AvatarDir, jpg)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	old, err := s.Store.SetAvatar(currentUser(r).ID, name)
	if err != nil {
		avatar.Remove(s.AvatarDir, name)
		s.serverError(w, r, err)
		return
	}
	avatar.Remove(s.AvatarDir, old)
	redirect(w, r, "/settings?saved=avatar")
}

func (s *Server) removeAvatar(w http.ResponseWriter, r *http.Request) {
	old, err := s.Store.SetAvatar(currentUser(r).ID, "")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	avatar.Remove(s.AvatarDir, old)
	redirect(w, r, "/settings?saved=avatar")
}

func (s *Server) savePassword(w http.ResponseWriter, r *http.Request) {
	me := currentUser(r)
	if !s.loginByUser.Allow(me.Username) {
		s.settingsError(w, r, "password", "Too many attempts. Wait a few minutes.")
		return
	}
	if ok, _, err := auth.VerifyPassword(r.FormValue("current"), me.PasswordHash, s.Params); err != nil || !ok {
		s.settingsError(w, r, "password", "Your current password is wrong.")
		return
	}
	pw := r.FormValue("new")
	if pw != r.FormValue("confirm") {
		s.settingsError(w, r, "password", "The new passwords don't match.")
		return
	}
	if err := auth.CheckPasswordPolicy(pw, me.Username, me.Email); err != nil {
		s.settingsError(w, r, "password", err.Error())
		return
	}
	hash, err := auth.HashPassword(pw, s.Params)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.Store.SetPasswordHash(me.ID, hash); err != nil {
		s.serverError(w, r, err)
		return
	}
	// Log out every other device.
	if c, err := r.Cookie(auth.SessionCookie); err == nil {
		s.Sessions.DeleteOthers(me.ID, c.Value)
	}
	redirect(w, r, "/settings?saved=password")
}

var avatarName = regexp.MustCompile(`^[0-9a-f]{32}\.jpg$`)

func (s *Server) serveAvatar(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !avatarName.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	// Names are random and change on every upload, so they can be cached forever.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(w, r, filepath.Join(s.AvatarDir, name))
}

// --- search ---

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	users, err := s.Store.SearchUsers(q, 20)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	groups, err := s.Store.SearchGroups(q, 20)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.page(w, r, http.StatusOK, "search", map[string]any{"Title": "Search", "Q": q, "Users": users, "Groups": groups})
}
