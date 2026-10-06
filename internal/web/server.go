// Package web is the HTTP layer: routing, middleware, handlers and templates.
package web

import (
	"log/slog"
	"net/http"
	"net/netip"
	"path/filepath"
	"time"

	"github.com/EmasXP/aotd/internal/auth"
	"github.com/EmasXP/aotd/internal/linker"
	"github.com/EmasXP/aotd/internal/musicbrainz"
	"github.com/EmasXP/aotd/internal/spotify"
	"github.com/EmasXP/aotd/internal/store"
)

type Server struct {
	Store     *store.Store
	Sessions  *auth.Sessions
	MB        *musicbrainz.Client
	Spotify   *spotify.Client // oEmbed only unless given credentials
	Linker    *linker.Linker  // optional; kicked when an album is posted
	Dev       bool            // allow non-Secure cookies over plain HTTP
	AvatarDir string
	Params    auth.Params
	Log       *slog.Logger
	// TrustedProxies may set the client IP via X-Forwarded-For.
	TrustedProxies []netip.Prefix
	tmpl           *templates
	dummyHash      string
	loginByIP      *auth.Limiter
	loginByUser    *auth.Limiter
	signupByIP     *auth.Limiter
}

func New(st *store.Store, mb *musicbrainz.Client, dataDir string, dev bool) (*Server, error) {
	t, err := loadTemplates()
	if err != nil {
		return nil, err
	}
	s := &Server{
		Store:     st,
		Sessions:  &auth.Sessions{DB: st.DB, Cache: st.Cache},
		MB:        mb,
		Spotify:   spotify.New("", ""),
		Dev:       dev,
		AvatarDir: filepath.Join(dataDir, "avatars"),
		Params:    auth.DefaultParams,
		Log:       slog.Default(),
		tmpl:      t,
		// 10 attempts, then one more per minute; per username 5, then one per 2 minutes.
		loginByIP:   auth.NewLimiter(time.Minute, 10),
		loginByUser: auth.NewLimiter(2*time.Minute, 5),
		signupByIP:  auth.NewLimiter(10*time.Minute, 5),
	}
	// Used to spend the same time on unknown usernames as on wrong passwords.
	if s.dummyHash, err = auth.HashPassword("not a real password", s.Params); err != nil {
		return nil, err
	}
	return s, nil
}

// Handler returns the fully wrapped HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /static/", http.StripPrefix("/static/", staticHandler()))
	mux.HandleFunc("GET /avatars/{name}", s.serveAvatar)

	mux.HandleFunc("GET /signup", s.signupForm)
	mux.HandleFunc("POST /signup", s.signup)
	mux.HandleFunc("GET /login", s.loginForm)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.logout)

	authed := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.requireUser(h)) }

	authed("GET /{$}", s.wall)
	authed("GET /mb/search", s.mbSearch)
	authed("POST /posts", s.createPost)
	authed("GET /posts/{id}", s.showPost)
	authed("POST /posts/{id}/note", s.updateNote)
	authed("POST /posts/{id}/release", s.matchRelease)
	authed("DELETE /posts/{id}", s.deletePost)
	authed("POST /posts/{id}/checkin", s.checkIn)
	authed("DELETE /posts/{id}/checkin", s.undoCheckIn)
	authed("POST /posts/{id}/comments", s.addComment)
	authed("DELETE /comments/{id}", s.deleteComment)

	authed("GET /u/{username}", s.profile)
	authed("GET /u/{username}/followers", s.followers)
	authed("GET /u/{username}/following", s.following)
	authed("POST /u/{username}/follow", s.follow)
	authed("DELETE /u/{username}/follow", s.unfollow)

	authed("GET /settings", s.settings)
	authed("POST /settings/profile", s.saveProfile)
	authed("POST /settings/avatar", s.saveAvatar)
	authed("POST /settings/avatar/delete", s.removeAvatar)
	authed("POST /settings/password", s.savePassword)

	authed("GET /search", s.search)

	authed("GET /groups", s.groups)
	authed("GET /groups/new", s.newGroupForm)
	authed("POST /groups", s.createGroup)
	authed("GET /g/{slug}", s.showGroup)
	authed("GET /g/{slug}/edit", s.editGroupForm)
	authed("POST /g/{slug}/edit", s.updateGroup)
	authed("POST /g/{slug}/delete", s.deleteGroup)
	authed("POST /g/{slug}/members", s.joinGroup)
	authed("DELETE /g/{slug}/members", s.leaveGroup)

	var h http.Handler = mux
	h = s.loadUser(h)
	h = limitBody(h)
	h = http.NewCrossOriginProtection().Handler(h)
	h = securityHeaders(h)
	h = s.recoverer(h)
	h = s.logRequests(h)
	return h
}
