package web

import (
	"net/http"
	"net/netip"
	"strings"
)

// clientIP returns the address used for rate limiting.
//
// Forwarding headers are only believed when the TCP peer is a trusted proxy;
// otherwise anyone could pick their own IP and dodge the login limits.
// X-Forwarded-For is read right to left, skipping trusted proxies, so that
// entries a client prepended itself are ignored. The first untrusted address
// is the client.
func (s *Server) clientIP(r *http.Request) string {
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	ip := peer.Addr().Unmap()
	if !s.trusted(ip) {
		return ip.String()
	}
	if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
		hops := strings.Split(strings.Join(xff, ","), ",")
		for i := len(hops) - 1; i >= 0; i-- {
			hop, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
			if err != nil {
				break // garbage: stop at the last address we could verify
			}
			ip = hop.Unmap()
			if !s.trusted(ip) {
				break
			}
		}
		return ip.String()
	}
	if real, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get("X-Real-IP"))); err == nil {
		return real.Unmap().String()
	}
	return ip.String()
}

func (s *Server) trusted(ip netip.Addr) bool {
	for _, p := range s.TrustedProxies {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}
