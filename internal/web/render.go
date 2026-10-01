package web

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"hash/fnv"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/EmasXP/aotd/internal/day"
	"github.com/EmasXP/aotd/internal/model"
	"github.com/EmasXP/aotd/internal/store"
)

//go:embed templates static
var assets embed.FS

func staticHandler() http.Handler {
	sub, _ := fs.Sub(assets, "static")
	fsrv := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		fsrv.ServeHTTP(w, r)
	})
}

// templates holds one template set per page, each combining the layout,
// all partials and the page itself.
type templates struct {
	pages    map[string]*template.Template
	partials *template.Template
}

var funcs = template.FuncMap{
	"prettyDate": day.Pretty,
	"ago":        ago,
	"initials":   initials,
	"hue":        hue,
	"plural": func(n any, one, many string) string {
		if fmt.Sprint(n) == "1" {
			return one
		}
		return many
	},
	"dict": func(kv ...any) (map[string]any, error) {
		if len(kv)%2 != 0 {
			return nil, errors.New("dict: odd number of args")
		}
		m := make(map[string]any, len(kv)/2)
		for i := 0; i < len(kv); i += 2 {
			k, ok := kv[i].(string)
			if !ok {
				return nil, errors.New("dict: keys must be strings")
			}
			m[k] = kv[i+1]
		}
		return m, nil
	},
	"avatarURL": func(v any) string {
		var u model.User
		switch v := v.(type) {
		case model.User:
			u = v
		case *model.User:
			u = *v
		}
		if u.AvatarPath == "" {
			return ""
		}
		return "/avatars/" + u.AvatarPath
	},
	"deref": func(p *uint) uint {
		if p == nil {
			return 0
		}
		return *p
	},
}

func loadTemplates() (*templates, error) {
	partials, err := template.New("").Funcs(funcs).ParseFS(assets, "templates/partials/*.html")
	if err != nil {
		return nil, err
	}
	pages, err := fs.Glob(assets, "templates/pages/*.html")
	if err != nil {
		return nil, err
	}
	t := &templates{pages: map[string]*template.Template{}, partials: partials}
	for _, p := range pages {
		pt, err := template.Must(partials.Clone()).ParseFS(assets, "templates/layout.html", p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		t.pages[strings.TrimSuffix(path.Base(p), ".html")] = pt
	}
	return t, nil
}

// page renders a full page. data is merged with the common fields.
func (s *Server) page(w http.ResponseWriter, r *http.Request, status int, name string, data map[string]any) {
	t, ok := s.tmpl.pages[name]
	if !ok {
		s.serverError(w, r, fmt.Errorf("no page template %q", name))
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	data["Me"] = currentUser(r)
	data["Path"] = r.URL.Path
	data["Today"] = s.Store.Today()
	s.execute(w, r, status, t, "layout", data)
}

// partial renders a named partial template, e.g. for htmx swaps.
func (s *Server) partial(w http.ResponseWriter, r *http.Request, name string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	data["Me"] = currentUser(r)
	s.execute(w, r, http.StatusOK, s.tmpl.partials, name, data)
}

func (s *Server) execute(w http.ResponseWriter, r *http.Request, status int, t *template.Template, name string, data any) {
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Vary", "HX-Request")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	s.Log.Error("server error", "err", err, "path", r.URL.Path)
	http.Error(w, "Something went wrong. Please try again.", http.StatusInternalServerError)
}

// fail maps store errors to responses.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var ve store.ValidationError
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.notFound(w, r)
	case errors.Is(err, store.ErrForbidden):
		http.Error(w, "You can't do that.", http.StatusForbidden)
	case errors.As(err, &ve), errors.Is(err, store.ErrAlreadyPosted):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	default:
		s.serverError(w, r, err)
	}
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.page(w, r, http.StatusNotFound, "notfound", map[string]any{"Title": "Not found"})
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return t.In(day.Location).Format("2 Jan 2006")
}

func initials(s string) string {
	var out []rune
	for _, f := range strings.Fields(s) {
		out = append(out, []rune(strings.ToUpper(f))[0])
		if len(out) == 2 {
			break
		}
	}
	return string(out)
}

// hue picks one of 12 stable colour classes (h0–h11) for placeholders like
// avatars without a picture and missing cover art. Classes, not inline
// styles, because the CSP disallows inline styles.
func hue(s string) int {
	h := fnv.New32a()
	h.Write([]byte(s))
	return int(h.Sum32() % 12)
}
