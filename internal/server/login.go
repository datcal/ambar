package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/datcal/ambar/internal/audit"
	"github.com/datcal/ambar/internal/auth"
	"github.com/datcal/ambar/internal/httpx"
)

// loginFailedMessage is the only failure message the login form ever shows.
//
// §11: no user-enumeration difference between "no such user" and "wrong
// password". That includes the rate-limit case — telling an attacker they have
// found a real account by locking it differently would leak the same thing.
const loginFailedMessage = "Incorrect username or password."

const rateLimitedMessage = "Too many attempts. Try again later."

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	next := auth.SafeNextPath(r.URL.Query().Get("next"))

	// Already signed in: nothing to do here.
	if _, ok := auth.UserFromContext(r.Context()); ok {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}

	data := s.newPageData(r)
	data.Next = next
	s.render(w, r, "login.html", http.StatusOK, data)
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	// CSRF is already enforced by the middleware chain.
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form submission", http.StatusBadRequest)
		return
	}

	var (
		ctx      = r.Context()
		username = auth.NormalizeUsername(r.PostFormValue("username"))
		password = r.PostFormValue("password")
		next     = auth.SafeNextPath(r.PostFormValue("next"))
		ip       = httpx.ClientIPString(ctx)
	)

	reject := func(status int, message string) {
		data := s.newPageData(r)
		data.Next = next
		data.Error = message
		// Echoed back so a typo in the password does not also mean retyping the
		// username. Contextually escaped by html/template.
		data.Username = username
		s.render(w, r, "login.html", status, data)
	}

	// §11: rate limit by IP and by username, both consulted before any
	// expensive work. Checked before the password is verified so a locked-out
	// attacker cannot even use the argon2id cost as a signal.
	ipKey := ip
	if ipKey == "" {
		// An unresolvable peer still gets a bucket, shared with other
		// unresolvable peers, rather than an exemption.
		ipKey = "unknown"
	}
	if ok, _ := s.loginByIP.Allowed(ipKey); !ok {
		s.audit.Record(ctx, audit.Entry{
			Action: audit.ActionLoginFailed, Entity: "user", EntityID: username, IP: ip,
			Detail: map[string]any{"reason": "rate_limited", "scope": "ip"},
		})
		reject(http.StatusTooManyRequests, rateLimitedMessage)
		return
	}
	if ok, _ := s.loginByUser.Allowed(username); !ok {
		s.audit.Record(ctx, audit.Entry{
			Action: audit.ActionLoginFailed, Entity: "user", EntityID: username, IP: ip,
			Detail: map[string]any{"reason": "rate_limited", "scope": "username"},
		})
		reject(http.StatusTooManyRequests, rateLimitedMessage)
		return
	}

	// Missing fields still take the failure path, so an empty submission is not
	// measurably faster than a wrong password.
	user, err := s.users.ByUsername(ctx, username)
	switch {
	case errors.Is(err, auth.ErrUserNotFound):
		// §11: verify against a throwaway hash so a nonexistent account costs
		// the same wall-clock time as a real one. Result deliberately discarded.
		_, _ = auth.VerifyPassword(s.dummyHash, password)
		s.failLogin(ctx, nil, username, ip, "unknown_user", ipKey)
		reject(http.StatusUnauthorized, loginFailedMessage)
		return
	case err != nil:
		s.log.ErrorContext(ctx, "user lookup failed during login", "error", err)
		reject(http.StatusInternalServerError, "Something went wrong. Try again.")
		return
	}

	ok, err := auth.VerifyPassword(user.PasswordHash, password)
	if err != nil {
		// A stored hash this code cannot read is a corrupt row, not a wrong
		// password. Never treat it as a success.
		s.log.ErrorContext(ctx, "stored password hash is unreadable",
			"user_id", user.ID, "error", err)
		s.failLogin(ctx, &user.ID, username, ip, "unreadable_hash", ipKey)
		reject(http.StatusUnauthorized, loginFailedMessage)
		return
	}
	if !ok {
		s.failLogin(ctx, &user.ID, username, ip, "wrong_password", ipKey)
		reject(http.StatusUnauthorized, loginFailedMessage)
		return
	}

	// Success. Clear both buckets so earlier typos do not count against the
	// next login.
	s.loginByIP.Reset(ipKey)
	s.loginByUser.Reset(username)

	// §11: rotate on login. Any pre-login session token is discarded rather
	// than adopted, which is what defeats session fixation.
	var oldToken string
	if cookie, err := r.Cookie(auth.SessionCookieName); err == nil {
		oldToken = cookie.Value
	}

	token, _, err := s.sessions.Rotate(ctx, oldToken, user.ID, r.UserAgent(), ip)
	if err != nil {
		s.log.ErrorContext(ctx, "could not create session", "user_id", user.ID, "error", err)
		reject(http.StatusInternalServerError, "Something went wrong. Try again.")
		return
	}

	auth.SetSessionCookie(w, token, s.cfg.CookieSecure, s.sessions.AbsoluteTTL())

	if err := s.users.TouchLogin(ctx, user.ID); err != nil {
		// Cosmetic; the login itself has already succeeded.
		s.log.WarnContext(ctx, "could not record last_login_at", "user_id", user.ID, "error", err)
	}

	s.audit.Record(ctx, audit.Entry{
		UserID: &user.ID, Action: audit.ActionLoginSucceeded,
		Entity: "user", EntityID: user.Username, IP: ip,
	})
	s.log.InfoContext(ctx, "login succeeded", "user", user.Username, "ip", ip)

	http.Redirect(w, r, next, http.StatusSeeOther)
}

// failLogin records a failed attempt in both the limiter and the audit log.
func (s *Server) failLogin(ctx context.Context, userID *int64, username, ip, reason, ipKey string) {
	s.loginByIP.RecordFailure(ipKey)
	s.loginByUser.RecordFailure(username)
	s.audit.Record(ctx, audit.Entry{
		UserID: userID, Action: audit.ActionLoginFailed,
		Entity: "user", EntityID: username, IP: ip,
		Detail: map[string]any{"reason": reason},
	})
	// The reason is safe to log; it is only unsafe to send to the client.
	s.log.WarnContext(ctx, "login failed", "username", username, "reason", reason, "ip", ip)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if cookie, err := r.Cookie(auth.SessionCookieName); err == nil && cookie.Value != "" {
		if err := s.sessions.Destroy(ctx, cookie.Value); err != nil {
			s.log.ErrorContext(ctx, "could not delete session on logout", "error", err)
		}
	}
	auth.ClearSessionCookie(w, s.cfg.CookieSecure)

	if user, ok := auth.UserFromContext(ctx); ok {
		s.audit.Record(ctx, audit.Entry{
			UserID: &user.ID, Action: audit.ActionLogout,
			Entity: "user", EntityID: user.Username, IP: httpx.ClientIPString(ctx),
		})
	}

	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
