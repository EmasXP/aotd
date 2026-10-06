// Package config loads runtime configuration from environment variables.
package config

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	Addr      string // listen address, e.g. ":8080"
	DSN       string // "postgres://..." or a SQLite file path
	DataDir   string // avatars and the default SQLite database live here
	CachePath string // bbolt cache file, recreated empty at every start
	Dev       bool   // disables Secure cookies so plain http://localhost works
	MBContact string // contact info in the MusicBrainz User-Agent
	// Spotify Web API app credentials, optional. Without them pasted Spotify
	// links give only title and cover (via oEmbed), not artist and year.
	SpotifyClientID, SpotifyClientSecret string
	// TrustedProxies are reverse proxies whose X-Forwarded-For header is
	// believed. Empty means the TCP peer address is always the client.
	TrustedProxies []netip.Prefix
}

func Load() (Config, error) {
	c := Config{
		Addr:      env("AOTD_ADDR", ":8080"),
		DataDir:   env("AOTD_DATA_DIR", "data"),
		Dev:       env("AOTD_DEV", "1") == "1",
		MBContact: env("AOTD_MB_CONTACT", "https://github.com/EmasXP/aotd"),

		SpotifyClientID:     os.Getenv("AOTD_SPOTIFY_CLIENT_ID"),
		SpotifyClientSecret: os.Getenv("AOTD_SPOTIFY_CLIENT_SECRET"),
	}
	if (c.SpotifyClientID == "") != (c.SpotifyClientSecret == "") {
		return Config{}, fmt.Errorf("set both AOTD_SPOTIFY_CLIENT_ID and AOTD_SPOTIFY_CLIENT_SECRET, or neither")
	}
	c.DSN = env("AOTD_DB_DSN", filepath.Join(c.DataDir, "aotd.db"))
	c.CachePath = env("AOTD_CACHE_PATH", filepath.Join(c.DataDir, "cache.db"))
	var err error
	if c.TrustedProxies, err = ParsePrefixes(os.Getenv("AOTD_TRUSTED_PROXIES")); err != nil {
		return Config{}, fmt.Errorf("AOTD_TRUSTED_PROXIES: %w", err)
	}
	return c, nil
}

// ParsePrefixes parses a comma-separated list of IPs and CIDRs, e.g.
// "10.0.0.5, 172.16.0.0/12, ::1". A bare IP is a single-address prefix.
func ParsePrefixes(list string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.Split(list, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if strings.Contains(f, "/") {
			p, err := netip.ParsePrefix(f)
			if err != nil {
				return nil, err
			}
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(f)
		if err != nil {
			return nil, err
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}
