package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/EmasXP/aotd/internal/auth"
	"github.com/EmasXP/aotd/internal/model"
	"github.com/EmasXP/aotd/internal/spotify"
)

type ctxKey int

const userKey ctxKey = iota

// currentUser returns the logged-in user, or nil.
func currentUser(r *http.Request) *model.User {
	u, _ := r.Context().Value(userKey).(*model.User)
	return u
}

func (s *Server) loadUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(auth.SessionCookie)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		uid, renewed, err := s.Sessions.Lookup(c.Value)
		var u *model.User
		if err == nil {
			u, err = s.Store.UserByID(uid)
		}
		if err != nil {
			s.clearSessionCookie(w)
			next.ServeHTTP(w, r)
			return
		}
		if !renewed.IsZero() {
			s.setSessionCookie(w, c.Value, renewed)
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
	})
}

func (s *Server) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if currentUser(r) != nil {
			next.ServeHTTP(w, r)
			return
		}
		target := "/login"
		if r.Method == http.MethodGet && r.URL.RequestURI() != "/" {
			target += "?next=" + url.QueryEscape(r.URL.RequestURI())
		}
		redirect(w, r, target)
	})
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: auth.SessionCookie, Value: token, Path: "/", Expires: expires,
		MaxAge: int(time.Until(expires).Seconds()), HttpOnly: true, Secure: !s.Dev, SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: auth.SessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: !s.Dev, SameSite: http.SameSiteLaxMode,
	})
}

// redirect sends the client elsewhere. htmx fragment requests can't follow a
// normal redirect into a full page, so they get HX-Redirect instead.
func redirect(w http.ResponseWriter, r *http.Request, target string) {
	if isFragment(r) {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// isFragment reports whether htmx asked for a partial (not a boosted page).
func isFragment(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-Boosted") != "true"
}

// safeNext only allows local paths, so ?next= can't be an open redirect.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, `/\`) {
		return "/"
	}
	return next
}

func securityHeaders(next http.Handler) http.Handler {
	csp := strings.Join([]string{
		"default-src 'self'",
		"img-src 'self' https://coverartarchive.org https://archive.org https://*.archive.org " + strings.Join(spotify.CoverHosts, " ") + " data:",
		"script-src 'self'",
		"style-src 'self'",
		"object-src 'none'",
		"base-uri 'none'",
		"form-action 'self'",
		"frame-ancestors 'none'",
	}, "; ")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// limitBody caps request bodies: small for forms, larger for avatar uploads.
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := int64(64 << 10)
		if r.URL.Path == "/settings/avatar" {
			limit = 3 << 20
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.Log.Error("panic", "err", v, "path", r.URL.Path)
				http.Error(w, "Internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		if strings.HasPrefix(r.URL.Path, "/static/") {
			return
		}
		s.Log.Info("request", "method", r.Method, "path", r.URL.Path, "status", sw.status, "dur", time.Since(start).Round(time.Millisecond))
	})
}
