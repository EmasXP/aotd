package web

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/EmasXP/aotd/internal/auth"
	"github.com/EmasXP/aotd/internal/model"
	"github.com/EmasXP/aotd/internal/store"
)

func (s *Server) signupForm(w http.ResponseWriter, r *http.Request) {
	if currentUser(r) != nil {
		redirect(w, r, "/")
		return
	}
	s.page(w, r, http.StatusOK, "signup", map[string]any{"Title": "Sign up"})
}

func (s *Server) signup(w http.ResponseWriter, r *http.Request) {
	form := map[string]any{
		"Title":       "Sign up",
		"Username":    r.FormValue("username"),
		"Email":       r.FormValue("email"),
		"DisplayName": r.FormValue("display_name"),
	}
	fail := func(status int, msg string) {
		form["Error"] = msg
		s.page(w, r, status, "signup", form)
	}
	if !s.signupByIP.Allow(s.clientIP(r)) {
		fail(http.StatusTooManyRequests, "Too many sign-ups from your network. Try again later.")
		return
	}
	username, email, displayName, err := store.NormalizeSignup(r.FormValue("username"), r.FormValue("email"), r.FormValue("display_name"))
	if err != nil {
		fail(http.StatusUnprocessableEntity, err.Error())
		return
	}
	password := r.FormValue("password")
	if password != r.FormValue("password_confirm") {
		fail(http.StatusUnprocessableEntity, "The passwords don't match.")
		return
	}
	if err := auth.CheckPasswordPolicy(password, username, email); err != nil {
		fail(http.StatusUnprocessableEntity, err.Error())
		return
	}
	hash, err := auth.HashPassword(password, s.Params)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	u, err := s.Store.CreateUser(username, email, displayName, hash)
	if errors.Is(err, store.ErrUsernameTaken) || errors.Is(err, store.ErrEmailTaken) {
		fail(http.StatusConflict, err.Error())
		return
	} else if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.startSession(w, r, u, "/")
}

func (s *Server) loginForm(w http.ResponseWriter, r *http.Request) {
	if currentUser(r) != nil {
		redirect(w, r, "/")
		return
	}
	s.page(w, r, http.StatusOK, "login", map[string]any{"Title": "Log in", "Next": safeNext(r.URL.Query().Get("next"))})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	login := strings.ToLower(strings.TrimSpace(r.FormValue("login")))
	password := r.FormValue("password")
	next := safeNext(r.FormValue("next"))
	fail := func(status int, msg string) {
		s.page(w, r, status, "login", map[string]any{"Title": "Log in", "Login": login, "Next": next, "Error": msg})
	}
	if !s.loginByIP.Allow(s.clientIP(r)) || !s.loginByUser.Allow(login) {
		fail(http.StatusTooManyRequests, "Too many login attempts. Wait a few minutes and try again.")
		return
	}
	if len(password) > auth.MaxPasswordLen*4 { // bytes; don't hash megabytes
		fail(http.StatusUnauthorized, "Wrong username/email or password.")
		return
	}
	u, err := s.Store.UserByLogin(login)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.serverError(w, r, err)
		return
	}
	hash := s.dummyHash
	if u != nil && err == nil {
		hash = u.PasswordHash
	}
	ok, rehash, verr := auth.VerifyPassword(password, hash, s.Params)
	if err != nil || verr != nil || !ok {
		fail(http.StatusUnauthorized, "Wrong username/email or password.")
		return
	}
	if rehash {
		if h, err := auth.HashPassword(password, s.Params); err == nil {
			s.Store.SetPasswordHash(u.ID, h)
		}
	}
	s.startSession(w, r, u, next)
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u *model.User, next string) {
	// Drop any session the browser already had, so a planted cookie can't
	// carry over (session fixation).
	if c, err := r.Cookie(auth.SessionCookie); err == nil {
		s.Sessions.Delete(c.Value)
	}
	token, expires, err := s.Sessions.Create(u.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.setSessionCookie(w, token, expires)
	redirect(w, r, next)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(auth.SessionCookie); err == nil {
		s.Sessions.Delete(c.Value)
	}
	s.clearSessionCookie(w)
	redirect(w, r, "/login")
}

// PurgeSessions periodically deletes expired sessions.
func (s *Server) PurgeSessions(every time.Duration) {
	for range time.Tick(every) {
		if err := s.Sessions.PurgeExpired(); err != nil {
			s.Log.Warn("purge sessions", "err", err)
		}
	}
}
