package api

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// clientIPMiddleware rewrites r.RemoteAddr to the client address a
// trusted reverse proxy reports, so the access log shows who actually
// made the request. Only peers inside trusted are believed; anyone
// else could put anything in X-Forwarded-For. With no trusted proxies
// it is a no-op.
func clientIPMiddleware(trusted []netip.Prefix) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if len(trusted) == 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ip, ok := forwardedClientIP(r, trusted); ok {
				r = r.WithContext(r.Context())
				r.RemoteAddr = ip.String()
			}
			next.ServeHTTP(w, r)
		})
	}
}

// forwardedClientIP returns the client address for a request that
// came through a trusted proxy. X-Forwarded-For is read right to
// left, skipping trusted hops: the first untrusted address is the
// client (anything left of it was supplied by the client itself). If
// every hop is trusted, the leftmost is taken. X-Real-IP is the
// fallback for proxies that only set that.
func forwardedClientIP(r *http.Request, trusted []netip.Prefix) (netip.Addr, bool) {
	peer, ok := parseAddr(r.RemoteAddr)
	if !ok || !inPrefixes(peer, trusted) {
		return netip.Addr{}, false
	}
	if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
		hops := strings.Split(strings.Join(xff, ","), ",")
		var leftmost netip.Addr
		for i := len(hops) - 1; i >= 0; i-- {
			a, ok := parseAddr(strings.TrimSpace(hops[i]))
			if !ok {
				break
			}
			if !inPrefixes(a, trusted) {
				return a, true
			}
			leftmost = a
		}
		if leftmost.IsValid() {
			return leftmost, true
		}
	}
	if a, ok := parseAddr(strings.TrimSpace(r.Header.Get("X-Real-IP"))); ok {
		return a, true
	}
	return netip.Addr{}, false
}

// parseAddr accepts "ip", "ip:port" and "[ipv6]:port".
func parseAddr(s string) (netip.Addr, bool) {
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

func inPrefixes(a netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
