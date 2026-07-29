package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

type (
	userContextKey    struct{}
	sessionContextKey struct{}
)

// Authenticator loads the session on every request. It never rejects anything —
// that is RequireUser's job, so public routes can share the same chain.
type Authenticator struct {
	sessions *SessionStore
	secure   bool
	log      *slog.Logger
}

func NewAuthenticator(sessions *SessionStore, secure bool, log *slog.Logger) *Authenticator {
	return &Authenticator{sessions: sessions, secure: secure, log: log}
}

// Load attaches the user and session to the request context when the cookie
// names a valid session.
func (a *Authenticator) Load(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(SessionCookieName)
		if err != nil || cookie.Value == "" {
			next.ServeHTTP(w, r)
			return
		}

		sess, user, err := a.sessions.Lookup(r.Context(), cookie.Value)
		switch {
		case errors.Is(err, ErrNoSession):
			// Expired or unknown. Clear it so the browser stops sending a dead
			// cookie on every request.
			ClearSessionCookie(w, a.secure)
			next.ServeHTTP(w, r)
			return
		case err != nil:
			// A database problem, not an authentication decision. Log it and
			// continue unauthenticated; RequireUser will send them to /login.
			a.log.ErrorContext(r.Context(), "session lookup failed", "error", err)
			next.ServeHTTP(w, r)
			return
		}

		ctx := context.WithValue(r.Context(), userContextKey{}, user)
		ctx = context.WithValue(ctx, sessionContextKey{}, sess)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireUser gates a route on being logged in.
func RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := UserFromContext(r.Context()); ok {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		// 303 rather than 302, so a rejected POST is retried as a GET.
		http.Redirect(w, r, "/login?next="+url.QueryEscape(SafeNextPath(r.URL.RequestURI())), http.StatusSeeOther)
	})
}

// UserFromContext returns the logged-in user, if any.
func UserFromContext(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(userContextKey{}).(User)
	return u, ok
}

// SessionFromContext returns the current session, if any.
func SessionFromContext(ctx context.Context) (Session, bool) {
	s, ok := ctx.Value(sessionContextKey{}).(Session)
	return s, ok
}

// SafeNextPath sanitises a post-login redirect target.
//
// Without this, /login?next=https://evil.example is an open redirect that
// borrows this application's credibility for a phishing page. Only a local,
// single-slash, absolute path survives.
func SafeNextPath(next string) string {
	const fallback = "/"
	if next == "" {
		return fallback
	}
	// Reject anything that could be read as scheme-relative or absolute-URL.
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return fallback
	}
	if strings.Contains(next, "\\") || strings.ContainsAny(next, "\r\n") {
		return fallback
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return fallback
	}
	// Sending a logged-in user back to /login would loop.
	if u.Path == "/login" || u.Path == "/logout" {
		return fallback
	}
	return u.RequestURI()
}
