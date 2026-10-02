package links

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		in   string
		want Link
	}{
		{"https://open.spotify.com/album/6dVIqQ8qmQ5GBnJ9shOYGE", Link{Spotify, "6dVIqQ8qmQ5GBnJ9shOYGE"}},
		{" https://open.spotify.com/intl-sv/album/6dVIqQ8qmQ5GBnJ9shOYGE?si=abc123 ", Link{Spotify, "6dVIqQ8qmQ5GBnJ9shOYGE"}},
		{"open.spotify.com/album/6dVIqQ8qmQ5GBnJ9shOYGE", Link{Spotify, "6dVIqQ8qmQ5GBnJ9shOYGE"}},
		{"spotify:album:6dVIqQ8qmQ5GBnJ9shOYGE", Link{Spotify, "6dVIqQ8qmQ5GBnJ9shOYGE"}},
		{"https://music.apple.com/gb/album/ok-computer/1097861387", Link{AppleMusic, "1097861387"}},
		{"https://music.apple.com/gb/album/1097861387", Link{AppleMusic, "1097861387"}},
		{"https://itunes.apple.com/us/album/id1097861387", Link{}}, // old "id" prefix: not supported
		{"https://www.deezer.com/album/14879699", Link{Deezer, "14879699"}},
		{"https://www.deezer.com/sv/album/14879699", Link{Deezer, "14879699"}},
		{"https://tidal.com/browse/album/58990510", Link{Tidal, "58990510"}},
		{"https://listen.tidal.com/album/58990510", Link{Tidal, "58990510"}},
		{"https://radiohead.bandcamp.com/album/OK-Computer", Link{Bandcamp, "radiohead/ok-computer"}},
		{"https://open.spotify.com/track/6dVIqQ8qmQ5GBnJ9shOYGE", Link{}},
		{"https://open.spotify.com/album/short", Link{}},
		{"https://evil.example/album/14879699", Link{}},
		{"javascript:alert(1)", Link{}},
		{"OK Computer", Link{}},
		{"", Link{}},
	}
	for _, tt := range tests {
		got, ok := Parse(tt.in)
		if ok != (tt.want != Link{}) || (ok && got != tt.want) {
			t.Errorf("Parse(%q) = %+v, %v; want %+v", tt.in, got, ok, tt.want)
		}
	}
}

func TestURLRoundTrips(t *testing.T) {
	for _, l := range []Link{
		{Spotify, "6dVIqQ8qmQ5GBnJ9shOYGE"},
		{AppleMusic, "1097861387"},
		{Deezer, "14879699"},
		{Tidal, "58990510"},
		{Bandcamp, "radiohead/ok-computer"},
	} {
		if got, ok := Parse(l.URL()); !ok || got != l {
			t.Errorf("Parse(%q) = %+v, %v", l.URL(), got, ok)
		}
		if l.Name() == "" {
			t.Errorf("%s has no name", l.Source)
		}
	}
}
