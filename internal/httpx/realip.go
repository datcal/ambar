// Package httpx holds small HTTP helpers that are not tied to one handler.
package httpx

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Default headers consulted when AMBAR_REAL_IP_HEADER is not set. Both are
// named in §2. Order matters: CF-Connecting-IP is a single address written by
// Cloudflare itself, so it is more trustworthy than the appendable XFF chain.
var defaultHeaders = []string{"CF-Connecting-IP", "X-Forwarded-For"}

// RealIP resolves the client address of a request.
//
// §2 and §11 are emphatic about this: with AMBAR_TRUSTED_PROXIES empty, all
// forwarded headers are ignored and the socket peer is used. This matters
// because the resolved address is the key for login rate limiting — if a
// spoofed header could set it, every attacker gets a fresh bucket per request
// and the §11 defence is silently disabled.
type RealIP struct {
	trusted []netip.Prefix
	headers []string
}

// New builds a resolver. header may be empty, in which case the defaults are
// consulted. trusted may be empty, in which case headers are never consulted.
func New(trusted []netip.Prefix, header string) *RealIP {
	headers := defaultHeaders
	if h := strings.TrimSpace(header); h != "" {
		headers = []string{h}
	}
	return &RealIP{trusted: trusted, headers: headers}
}

// ClientIP returns the best available client address.
//
// It falls back to the peer address on every ambiguity — an unparseable header,
// a chain of nothing but trusted hops, an untrusted peer. Falling back to the
// truth the kernel gave us is always safe; trusting a header is not.
func (r *RealIP) ClientIP(req *http.Request) netip.Addr {
	peer := peerAddr(req)

	// The whole mechanism is off unless proxies are explicitly configured.
	if len(r.trusted) == 0 {
		return peer
	}
	// A request that did not come from a configured proxy has no business
	// carrying forwarded headers, so ignore them.
	if !peer.IsValid() || !r.isTrusted(peer) {
		return peer
	}

	for _, name := range r.headers {
		value := req.Header.Get(name)
		if value == "" {
			continue
		}
		if addr, ok := r.fromChain(value); ok {
			return addr
		}
	}
	return peer
}

// fromChain walks a comma-separated forwarded chain from right to left and
// returns the first address that is not itself a trusted proxy.
//
// Right to left because the chain is appended to as it travels: everything to
// the left of the first untrusted hop was written by someone we do not trust and
// can be entirely fabricated.
func (r *RealIP) fromChain(value string) (netip.Addr, bool) {
	parts := strings.Split(value, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		addr, ok := parseAddr(strings.TrimSpace(parts[i]))
		if !ok {
			// A malformed hop means the chain cannot be reasoned about.
			return netip.Addr{}, false
		}
		if r.isTrusted(addr) {
			continue
		}
		return addr, true
	}
	// Every hop was a trusted proxy: there is no client address in here.
	return netip.Addr{}, false
}

func (r *RealIP) isTrusted(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, p := range r.trusted {
		// Compare in the prefix's own family; Contains is false across families.
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

type contextKey struct{}

// Middleware resolves the client address once per request and stores it in the
// context, so handlers and the access log agree on one answer.
func (r *RealIP) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx := context.WithValue(req.Context(), contextKey{}, r.ClientIP(req))
		next.ServeHTTP(w, req.WithContext(ctx))
	})
}

// ClientIPFromContext returns the address resolved by Middleware.
func ClientIPFromContext(ctx context.Context) netip.Addr {
	if addr, ok := ctx.Value(contextKey{}).(netip.Addr); ok {
		return addr
	}
	return netip.Addr{}
}

// ClientIPString is the form stored in sessions.ip and audit_log.ip. An invalid
// address becomes "" rather than a misleading placeholder.
func ClientIPString(ctx context.Context) string {
	if addr := ClientIPFromContext(ctx); addr.IsValid() {
		return addr.String()
	}
	return ""
}

func peerAddr(req *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		// httptest and unix sockets can leave RemoteAddr without a port.
		host = req.RemoteAddr
	}
	addr, _ := parseAddr(host)
	return addr
}

func parseAddr(s string) (netip.Addr, bool) {
	// Strip an IPv6 zone and brackets, both of which appear in real headers.
	s = strings.TrimPrefix(strings.TrimSuffix(s, "]"), "[")
	addr, err := netip.ParseAddr(s)
	if err != nil {
		// Some proxies append a port to a chain entry.
		if host, _, splitErr := net.SplitHostPort(s); splitErr == nil {
			if addr, err = netip.ParseAddr(host); err == nil {
				return addr.Unmap(), true
			}
		}
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}
