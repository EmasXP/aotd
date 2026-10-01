package web

import (
	"net/http/httptest"
	"testing"

	"github.com/EmasXP/aotd/internal/config"
)

func TestClientIP(t *testing.T) {
	proxies, err := config.ParsePrefixes("10.0.0.1, 172.16.0.0/12, ::1")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{TrustedProxies: proxies}
	tests := []struct {
		name, remote, xff, realIP, want string
	}{
		{"no proxy", "203.0.113.7:5000", "", "", "203.0.113.7"},
		{"untrusted peer can't spoof", "203.0.113.7:5000", "1.2.3.4", "1.2.3.4", "203.0.113.7"},
		{"trusted proxy", "10.0.0.1:5000", "198.51.100.9", "", "198.51.100.9"},
		{"client-prepended entries ignored", "10.0.0.1:5000", "1.2.3.4, 198.51.100.9", "", "198.51.100.9"},
		{"proxy chain inside CIDR", "10.0.0.1:5000", "198.51.100.9, 172.20.1.1", "", "198.51.100.9"},
		{"garbage stops the walk", "10.0.0.1:5000", "198.51.100.9, nonsense", "", "10.0.0.1"},
		{"all hops trusted", "10.0.0.1:5000", "172.16.0.5", "", "172.16.0.5"},
		{"X-Real-IP fallback", "10.0.0.1:5000", "", "198.51.100.9", "198.51.100.9"},
		{"trusted proxy without headers", "10.0.0.1:5000", "", "", "10.0.0.1"},
		{"IPv6 loopback proxy", "[::1]:5000", "2001:db8::42", "", "2001:db8::42"},
		{"IPv4-mapped peer", "[::ffff:10.0.0.1]:5000", "198.51.100.9", "", "198.51.100.9"},
	}
	for _, tt := range tests {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = tt.remote
		if tt.xff != "" {
			r.Header.Set("X-Forwarded-For", tt.xff)
		}
		if tt.realIP != "" {
			r.Header.Set("X-Real-IP", tt.realIP)
		}
		if got := s.clientIP(r); got != tt.want {
			t.Errorf("%s: clientIP = %s, want %s", tt.name, got, tt.want)
		}
	}
}

func TestParsePrefixesRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"10.0.0", "10.0.0.0/33", "proxy.local"} {
		if _, err := config.ParsePrefixes(bad); err == nil {
			t.Errorf("ParsePrefixes(%q) accepted", bad)
		}
	}
	if ps, err := config.ParsePrefixes(" , "); err != nil || len(ps) != 0 {
		t.Errorf("empty list = %v, %v", ps, err)
	}
}
