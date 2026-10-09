// Package links recognises album pages on streaming and download services,
// and turns them into a (source, ID) pair that identifies the album there.
// The canonical URL is rebuilt from the pair, so differently written links to
// the same album (country prefixes, tracking parameters, app URIs) compare
// equal.
package links

import (
	"net/url"
	"regexp"
	"strings"
)

const (
	Spotify    = "spotify"
	AppleMusic = "apple_music"
	Deezer     = "deezer"
	Tidal      = "tidal"
	Bandcamp   = "bandcamp"
)

// Sources lists every known source in display order.
var Sources = []string{Spotify, AppleMusic, Deezer, Tidal, Bandcamp}

var names = map[string]string{
	Spotify:    "Spotify",
	AppleMusic: "Apple Music",
	Deezer:     "Deezer",
	Tidal:      "Tidal",
	Bandcamp:   "Bandcamp",
}

// Link is an album on one source.
type Link struct {
	Source string
	ID     string
}

// Name is the source's display name.
func (l Link) Name() string { return names[l.Source] }

// URL is the canonical page for the album.
func (l Link) URL() string {
	switch l.Source {
	case Spotify:
		return "https://open.spotify.com/album/" + l.ID
	case AppleMusic:
		return "https://music.apple.com/album/" + l.ID
	case Deezer:
		return "https://www.deezer.com/album/" + l.ID
	case Tidal:
		return "https://tidal.com/album/" + l.ID
	case Bandcamp:
		sub, slug, _ := strings.Cut(l.ID, "/")
		return "https://" + sub + ".bandcamp.com/album/" + slug
	}
	return ""
}

// AppURI returns the deep-link protocol scheme to open the album in a native
// desktop app, or an empty string if the source has no desktop app protocol.
func (l Link) AppURI() string {
	switch l.Source {
	case Spotify:
		return "spotify:album:" + l.ID
	case AppleMusic:
		return "music://music.apple.com/album/" + l.ID
	case Deezer:
		return "deezer://album/" + l.ID
	case Tidal:
		return "tidal://album/" + l.ID
	}
	return ""
}

var (
	spotifyID   = regexp.MustCompile(`^[A-Za-z0-9]{22}$`)
	digits      = regexp.MustCompile(`^[0-9]{1,20}$`)
	locale      = regexp.MustCompile(`^(intl-)?[a-z]{2}([-_][a-zA-Z]{2,4})?$`) // "se", "intl-sv", "en-gb"
	bandcampSub = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)
	bandcampAlb = regexp.MustCompile(`^[a-z0-9-]{1,200}$`)
)

// Parse recognises an album link. It accepts what people paste: with or
// without scheme, country or language prefixes, query strings, and
// spotify:album: URIs.
func Parse(raw string) (Link, bool) {
	raw = strings.TrimSpace(raw)
	if id, ok := strings.CutPrefix(raw, "spotify:album:"); ok {
		return valid(Link{Spotify, id})
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return Link{}, false
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	parts := strings.FieldsFunc(u.Path, func(r rune) bool { return r == '/' })
	if len(parts) > 0 && locale.MatchString(parts[0]) && parts[0] != "album" {
		parts = parts[1:]
	}
	switch host {
	case "open.spotify.com", "play.spotify.com":
		if len(parts) == 2 && parts[0] == "album" {
			return valid(Link{Spotify, parts[1]})
		}
	case "music.apple.com", "itunes.apple.com":
		// /album/{slug}/{id} or /album/{id}
		if len(parts) >= 2 && parts[0] == "album" {
			return valid(Link{AppleMusic, parts[len(parts)-1]})
		}
	case "deezer.com":
		if len(parts) == 2 && parts[0] == "album" {
			return valid(Link{Deezer, parts[1]})
		}
	case "tidal.com", "listen.tidal.com":
		if len(parts) > 0 && parts[0] == "browse" {
			parts = parts[1:]
		}
		if len(parts) == 2 && parts[0] == "album" {
			return valid(Link{Tidal, parts[1]})
		}
	default:
		if sub, ok := strings.CutSuffix(host, ".bandcamp.com"); ok && len(parts) == 2 && parts[0] == "album" {
			return valid(Link{Bandcamp, sub + "/" + strings.ToLower(parts[1])})
		}
	}
	return Link{}, false
}

func valid(l Link) (Link, bool) {
	var ok bool
	switch l.Source {
	case Spotify:
		ok = spotifyID.MatchString(l.ID)
	case AppleMusic, Deezer, Tidal:
		ok = digits.MatchString(l.ID)
	case Bandcamp:
		sub, slug, _ := strings.Cut(l.ID, "/")
		ok = bandcampSub.MatchString(sub) && bandcampAlb.MatchString(slug)
	}
	return l, ok
}
