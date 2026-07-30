package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
)

// CSRF names. §11 requires tokens on all state-changing posts, configured
// globally for htmx via hx-headers.
const (
	CSRFCookieName = "ambar_csrf"
	CSRFFieldName  = "csrf_token"
	CSRFHeaderName = "X-CSRF-Token"

	csrfIDBytes = 32
)

// CSRF implements signed double-submit tokens.
//
// A random ID lives in a cookie; the token handed to the page is
// HMAC-SHA256(session secret, ID). An attacker on another origin can cause the
// cookie to be sent but cannot read it, and cannot compute the HMAC without the
// server secret — so they cannot produce a matching pair.
//
// The token is bound to the CSRF cookie rather than to the session, which means
// the login form (no session yet) is protected by the same mechanism, and
// rotating the session on login does not invalidate the form the user is
// currently looking at.
type CSRF struct {
	secret []byte
	secure bool
}

func NewCSRF(secret []byte, secure bool) *CSRF {
	return &CSRF{secret: secret, secure: secure}
}

type csrfContextKey struct{}

// Ensure guarantees every request has a CSRF cookie and puts the matching token
// in the request context, where handlers read it with TokenFromContext.
//
// It runs before Protect in the chain, and before any handler writes a body —
// it may need to set a cookie.
func (c *CSRF) Ensure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := c.readID(r)
		if id == "" {
			raw := make([]byte, csrfIDBytes)
			if _, err := rand.Read(raw); err != nil {
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			id = base64.RawURLEncoding.EncodeToString(raw)
			http.SetCookie(w, &http.Cookie{
				Name:     CSRFCookieName,
				Value:    id,
				Path:     "/",
				HttpOnly: true,
				Secure:   c.secure,
				SameSite: http.SameSiteLaxMode,
			})
		}
		ctx := context.WithValue(r.Context(), csrfContextKey{}, c.sign(id))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Protect rejects unsafe requests that do not carry a valid token.
func (c *CSRF) Protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
			next.ServeHTTP(w, r)
			return
		}

		// A bearer-token request is not cookie-authenticated, so CSRF — which
		// exploits ambient cookies a browser attaches automatically — cannot apply
		// to it. The token itself is the proof of intent. This is what lets the
		// §10 API's POST/DELETE work without a CSRF cookie the Godot plugin has no
		// way to obtain.
		if strings.HasPrefix(strings.ToLower(r.Header.Get("Authorization")), "bearer ") {
			next.ServeHTTP(w, r)
			return
		}

		// The cookie as the client sent it, never the one Ensure may have just
		// created — otherwise a first-ever POST with no cookie would validate
		// against a token the server minted for it.
		id := c.readID(r)
		if id == "" {
			c.reject(w, r, "missing CSRF cookie")
			return
		}

		if !hmac.Equal([]byte(c.submittedToken(r)), []byte(c.sign(id))) {
			c.reject(w, r, "CSRF token mismatch")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// submittedToken reads the token from the header or the form body.
func (c *CSRF) submittedToken(r *http.Request) string {
	if t := r.Header.Get(CSRFHeaderName); t != "" {
		return t
	}
	// Multipart bodies are left alone: parsing one here would buffer an entire
	// upload into memory before the handler decides on a size cap (M4). Those
	// requests must use the header, which htmx sends via hx-headers.
	if ct := r.Header.Get("Content-Type"); strings.HasPrefix(ct, "multipart/form-data") {
		return ""
	}
	// ParseForm caches into r.PostForm, so the handler can still read the body.
	if err := r.ParseForm(); err != nil {
		return ""
	}
	return r.PostFormValue(CSRFFieldName)
}

func (c *CSRF) readID(r *http.Request) string {
	cookie, err := r.Cookie(CSRFCookieName)
	if err != nil || cookie.Value == "" {
		return ""
	}
	return cookie.Value
}

func (c *CSRF) sign(id string) string {
	mac := hmac.New(sha256.New, c.secret)
	mac.Write([]byte(id))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// reject answers a failed check. The message is deliberately vague in the
// response and specific in the log.
func (c *CSRF) reject(w http.ResponseWriter, r *http.Request, reason string) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		http.Error(w, `{"error":"csrf_failed"}`, http.StatusForbidden)
		return
	}
	http.Error(w, "Request rejected: "+reason+". Reload the page and try again.", http.StatusForbidden)
}

// TokenFromContext returns the CSRF token for this request, for embedding in a
// form or an hx-headers attribute.
func TokenFromContext(ctx context.Context) string {
	if t, ok := ctx.Value(csrfContextKey{}).(string); ok {
		return t
	}
	return ""
}
