package httpx

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func prefixes(t *testing.T, cidrs ...string) []netip.Prefix {
	t.Helper()

	var out []netip.Prefix
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			t.Fatalf("bad test CIDR %q: %v", c, err)
		}
		out = append(out, p.Masked())
	}
	return out
}

// TestClientIP is the table that matters most in this package. The resolved
// address is the key for login rate limiting (§11), so a spoofable header here
// silently disables that defence.
func TestClientIP(t *testing.T) {
	tests := []struct {
		name    string
		trusted []string
		header  string
		peer    string
		headers map[string]string
		want    string
	}{
		// --- The default: no trusted proxies, so headers are ignored (§2) ---
		{
			name:    "no proxies configured, headers ignored",
			peer:    "203.0.113.7:54321",
			headers: map[string]string{"X-Forwarded-For": "1.2.3.4"},
			want:    "203.0.113.7",
		},
		{
			name:    "no proxies configured, CF header ignored",
			peer:    "203.0.113.7:54321",
			headers: map[string]string{"CF-Connecting-IP": "1.2.3.4"},
			want:    "203.0.113.7",
		},
		{
			name:    "no proxies configured, spoof attempt from a private peer",
			peer:    "10.0.0.9:1234",
			headers: map[string]string{"X-Forwarded-For": "9.9.9.9"},
			want:    "10.0.0.9",
		},

		// --- Untrusted peer: headers still ignored ---
		{
			name:    "peer outside the trusted set",
			trusted: []string{"10.0.0.0/8"},
			peer:    "203.0.113.7:54321",
			headers: map[string]string{"X-Forwarded-For": "1.2.3.4"},
			want:    "203.0.113.7",
		},

		// --- Trusted peer: headers honoured ---
		{
			name:    "trusted proxy, single hop",
			trusted: []string{"10.0.0.0/8"},
			peer:    "10.0.0.1:9999",
			headers: map[string]string{"X-Forwarded-For": "203.0.113.7"},
			want:    "203.0.113.7",
		},
		{
			name:    "trusted proxy, rightmost untrusted hop wins",
			trusted: []string{"10.0.0.0/8"},
			peer:    "10.0.0.1:9999",
			headers: map[string]string{"X-Forwarded-For": "1.1.1.1, 203.0.113.7, 10.0.0.2"},
			want:    "203.0.113.7",
		},
		{
			name:    "forged hops to the left are ignored",
			trusted: []string{"10.0.0.0/8"},
			peer:    "10.0.0.1:9999",
			headers: map[string]string{"X-Forwarded-For": "127.0.0.1, 8.8.8.8, 203.0.113.7"},
			want:    "203.0.113.7",
		},
		{
			name:    "chain of only trusted hops falls back to the peer",
			trusted: []string{"10.0.0.0/8"},
			peer:    "10.0.0.1:9999",
			headers: map[string]string{"X-Forwarded-For": "10.0.0.2, 10.0.0.3"},
			want:    "10.0.0.1",
		},
		{
			name:    "malformed hop falls back to the peer",
			trusted: []string{"10.0.0.0/8"},
			peer:    "10.0.0.1:9999",
			headers: map[string]string{"X-Forwarded-For": "not-an-ip"},
			want:    "10.0.0.1",
		},
		{
			name:    "empty header falls back to the peer",
			trusted: []string{"10.0.0.0/8"},
			peer:    "10.0.0.1:9999",
			headers: map[string]string{"X-Forwarded-For": ""},
			want:    "10.0.0.1",
		},
		{
			name:    "hop carrying a port is still parsed",
			trusted: []string{"10.0.0.0/8"},
			peer:    "10.0.0.1:9999",
			headers: map[string]string{"X-Forwarded-For": "203.0.113.7:44321"},
			want:    "203.0.113.7",
		},

		// --- Header precedence and configuration ---
		{
			name:    "CF-Connecting-IP is preferred over XFF",
			trusted: []string{"10.0.0.0/8"},
			peer:    "10.0.0.1:9999",
			headers: map[string]string{
				"CF-Connecting-IP": "203.0.113.9",
				"X-Forwarded-For":  "203.0.113.7",
			},
			want: "203.0.113.9",
		},
		{
			name:    "an explicitly configured header is the only one consulted",
			trusted: []string{"10.0.0.0/8"},
			header:  "X-Real-Ip",
			peer:    "10.0.0.1:9999",
			headers: map[string]string{
				"X-Real-Ip":       "203.0.113.5",
				"X-Forwarded-For": "1.2.3.4",
			},
			want: "203.0.113.5",
		},
		{
			name:    "configured header absent, others not consulted",
			trusted: []string{"10.0.0.0/8"},
			header:  "X-Real-Ip",
			peer:    "10.0.0.1:9999",
			headers: map[string]string{"X-Forwarded-For": "1.2.3.4"},
			want:    "10.0.0.1",
		},

		// --- IPv6, which a tailnet actually uses ---
		{
			name:    "ipv6 peer with no proxies",
			peer:    "[2001:db8::5]:443",
			headers: map[string]string{"X-Forwarded-For": "1.2.3.4"},
			want:    "2001:db8::5",
		},
		{
			name:    "ipv6 trusted proxy with ipv6 client",
			trusted: []string{"2001:db8::/32"},
			peer:    "[2001:db8::1]:443",
			// Outside the trusted /32, so this is a real client and not a hop.
			headers: map[string]string{"X-Forwarded-For": "2a00:1450::9"},
			want:    "2a00:1450::9",
		},
		{
			name:    "ipv6 client inside the trusted prefix is treated as a hop",
			trusted: []string{"2001:db8::/32"},
			peer:    "[2001:db8::1]:443",
			headers: map[string]string{"X-Forwarded-For": "2001:db8:1234::9"},
			want:    "2001:db8::1",
		},
		{
			name:    "ipv6 hop in brackets",
			trusted: []string{"10.0.0.0/8"},
			peer:    "10.0.0.1:9999",
			headers: map[string]string{"X-Forwarded-For": "[2001:db8::7]"},
			want:    "2001:db8::7",
		},
		{
			name: "ipv4-mapped ipv6 peer is normalised",
			peer: "[::ffff:203.0.113.7]:1234",
			want: "203.0.113.7",
		},
		{
			name:    "trust does not cross address families",
			trusted: []string{"10.0.0.0/8"},
			peer:    "[2001:db8::1]:443",
			headers: map[string]string{"X-Forwarded-For": "203.0.113.7"},
			want:    "2001:db8::1",
		},

		// --- Single-host trust, which is what a bare IP in config becomes ---
		{
			name:    "single-host prefix",
			trusted: []string{"172.17.0.1/32"},
			peer:    "172.17.0.1:5000",
			headers: map[string]string{"X-Forwarded-For": "203.0.113.7"},
			want:    "203.0.113.7",
		},
		{
			name:    "neighbour of a single-host prefix is not trusted",
			trusted: []string{"172.17.0.1/32"},
			peer:    "172.17.0.2:5000",
			headers: map[string]string{"X-Forwarded-For": "203.0.113.7"},
			want:    "172.17.0.2",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := New(prefixes(t, tc.trusted...), tc.header)

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tc.peer
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}

			got := r.ClientIP(req)
			if !got.IsValid() {
				t.Fatalf("ClientIP returned an invalid address for peer %q", tc.peer)
			}
			if got.String() != tc.want {
				t.Errorf("ClientIP = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestClientIPWithUnparseableRemoteAddr covers httptest and unix sockets, where
// RemoteAddr may have no port at all.
func TestClientIPWithUnparseableRemoteAddr(t *testing.T) {
	r := New(nil, "")

	for _, peer := range []string{"", "@", "not-an-address", "/tmp/socket"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = peer

		// The contract is only that this does not panic and reports invalid.
		if got := r.ClientIP(req); got.IsValid() {
			t.Errorf("peer %q resolved to %s, want an invalid address", peer, got)
		}
	}
}

func TestMiddlewareStoresTheResolvedAddress(t *testing.T) {
	r := New(prefixes(t, "10.0.0.0/8"), "")

	var got string
	handler := r.Middleware(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		got = ClientIPString(req.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:9999"
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if got != "203.0.113.7" {
		t.Errorf("ClientIPString = %q, want 203.0.113.7", got)
	}
}

// TestClientIPStringWithoutMiddleware: sessions.ip and audit_log.ip must get an
// empty string rather than a misleading placeholder.
func TestClientIPStringWithoutMiddleware(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := ClientIPString(req.Context()); got != "" {
		t.Errorf("ClientIPString = %q on a bare request, want empty", got)
	}
	if got := ClientIPFromContext(req.Context()); got.IsValid() {
		t.Errorf("ClientIPFromContext = %s, want invalid", got)
	}
}

// TestHeaderCasingIsIrrelevant: http.Header canonicalises names, and the
// configured value comes from an environment variable a human typed.
func TestHeaderCasingIsIrrelevant(t *testing.T) {
	for _, configured := range []string{"x-real-ip", "X-Real-IP", "X-REAL-IP"} {
		r := New(prefixes(t, "10.0.0.0/8"), configured)

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.1:9999"
		req.Header.Set("X-Real-Ip", "203.0.113.7")

		if got := r.ClientIP(req); got.String() != "203.0.113.7" {
			t.Errorf("configured header %q: ClientIP = %s, want 203.0.113.7", configured, got)
		}
	}
}
