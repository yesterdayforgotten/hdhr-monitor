package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// DefaultTrustedNets are the private and loopback ranges.
var DefaultTrustedNets = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("::1/128"),
}

// Headers a reverse proxy adds. A request carrying any of them came from
// outside through the proxy, even though the proxy itself is on the LAN.
var proxyHeaders = []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-IP", "Forwarded"}

// authorized lets trusted requests through, asks the rest for a login, and
// writes the refusal when it returns false.
func (s *Server) authorized(w http.ResponseWriter, r *http.Request) bool {
	if s.trusted(r) {
		return true
	}
	if s.cfg.Password == "" {
		http.Error(w, "Remote access is off: set HDHR_PASSWORD to allow it.", http.StatusForbidden)
		return false
	}
	user, pass, ok := r.BasicAuth()
	if ok && equal(user, s.cfg.Username) && equal(pass, s.cfg.Password) {
		return true
	}
	w.Header().Set("WWW-Authenticate", `Basic realm="HDHomeRun Monitor", charset="UTF-8"`)
	http.Error(w, "Login required", http.StatusUnauthorized)
	return false
}

func (s *Server) trusted(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	addr = addr.Unmap()

	if contains(s.cfg.TrustedProxies, addr) {
		// Our own proxy appends the address it got the request from as the
		// last X-Forwarded-For entry; anything before it came from the
		// client and can't be believed.
		xff := r.Header.Values("X-Forwarded-For")
		if len(xff) == 0 {
			return false
		}
		entries := strings.Split(xff[len(xff)-1], ",")
		client, err := netip.ParseAddr(strings.TrimSpace(entries[len(entries)-1]))
		if err != nil {
			return false
		}
		return contains(s.cfg.TrustedNets, client.Unmap())
	}

	for _, h := range proxyHeaders {
		if r.Header.Get(h) != "" {
			return false
		}
	}
	return contains(s.cfg.TrustedNets, addr)
}

func contains(nets []netip.Prefix, addr netip.Addr) bool {
	for _, p := range nets {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// equal compares in constant time, whatever the lengths.
func equal(a, b string) bool {
	ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}
