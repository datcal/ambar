package server

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/datcal/ambar/internal/audit"
	"github.com/datcal/ambar/internal/auth"
	"github.com/datcal/ambar/internal/httpx"
)

// Settings: users, and the links to everything else operational (M15).
//
// §11 is explicit that there is no self-registration and that the two users are
// equal, which until now meant the only way to add the second one was
// `ambar user add` on the server. That is a fine bootstrap and a poor answer to "my
// partner needs an account" — so any signed-in user can create another here, and
// every creation lands in the audit log with who did it.
//
// Everyone can change their own password here. Deliberately not offered: deleting a
// user or changing someone else's password. A deleted user's sessions, audit entries
// and manual tags all reference them, and §11's "two equal users" gives nobody the
// authority to lock the other out. Removing an account, and resetting a forgotten
// password, stay CLI decisions made on purpose (`ambar user passwd`).

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w, r, http.StatusOK, settingsErrors{}, r.URL.Query().Get("msg"))
}

// settingsErrors holds a refusal for whichever of the page's forms was submitted.
type settingsErrors struct {
	user     string // the "Add a user" form
	password string // the "Your password" form
}

// renderSettings draws the page with an optional form error and flash.
func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, status int, errs settingsErrors, flash string) {
	data := s.newPageData(r)
	data.Nav = "settings"
	data.UserError = errs.user
	data.PasswordError = errs.password
	data.Flash = flash

	users, err := s.users.List(r.Context())
	if err != nil {
		s.log.ErrorContext(r.Context(), "listing users failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	data.Users = users
	data.MinPasswordLength = auth.MinPasswordLength

	s.render(w, r, "settings.html", status, data)
}

// handleUserCreate adds a user (§11: no self-registration, so this is behind auth).
func (s *Server) handleUserCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderSettings(w, r, http.StatusBadRequest, settingsErrors{user: "Could not read the form."}, "")
		return
	}
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	confirm := r.PostFormValue("password_confirm")

	switch {
	case username == "":
		s.renderSettings(w, r, http.StatusBadRequest, settingsErrors{user: "A username is required."}, "")
		return
	case password != confirm:
		// Checked before the store so the message is about the mistake actually made.
		s.renderSettings(w, r, http.StatusBadRequest, settingsErrors{user: "The two passwords do not match."}, "")
		return
	}

	user, err := s.users.Create(r.Context(), username, password, auth.RoleUser)
	if err != nil {
		// The store's errors are already written for a human ("username is taken",
		// "password must be at least N characters"), so they are shown as they are.
		s.log.InfoContext(r.Context(), "user creation refused", "username", username, "error", err)
		s.renderSettings(w, r, http.StatusBadRequest, settingsErrors{user: capitalise(err.Error()) + "."}, "")
		return
	}

	var actor *int64
	if u, ok := auth.UserFromContext(r.Context()); ok {
		id := u.ID
		actor = &id
	}
	s.audit.Record(r.Context(), audit.Entry{
		UserID: actor, Action: audit.ActionUserCreated, Entity: "user",
		EntityID: user.Username, IP: httpx.ClientIPString(r.Context()),
		Detail: map[string]any{"role": user.Role, "created_via": "web"},
	})

	http.Redirect(w, r, "/settings?msg="+url.QueryEscape(
		"Created "+user.Username+". They can sign in now."), http.StatusSeeOther)
}

// handlePasswordChange changes the signed-in user's own password.
//
// The current password is asked for although the session already says who this is:
// a session can be borrowed — an unlocked laptop, a copied cookie — and turning that
// into the account itself should take the password. Wrong guesses count against the
// login limits (§11), so the form cannot be used to brute-force it instead of /login.
func (s *Server) handlePasswordChange(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u, ok := auth.UserFromContext(ctx)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	refuse := func(status int, message string) {
		s.renderSettings(w, r, status, settingsErrors{password: message}, "")
	}
	if err := r.ParseForm(); err != nil {
		refuse(http.StatusBadRequest, "Could not read the form.")
		return
	}

	var (
		current  = r.PostFormValue("current_password")
		password = r.PostFormValue("password")
		confirm  = r.PostFormValue("password_confirm")
		ip       = httpx.ClientIPString(ctx)
		ipKey    = ip
	)
	if ipKey == "" {
		ipKey = "unknown"
	}

	// The same buckets as /login, consulted before the argon2id work.
	if ok, _ := s.loginByIP.Allowed(ipKey); !ok {
		refuse(http.StatusTooManyRequests, rateLimitedMessage)
		return
	}
	if ok, _ := s.loginByUser.Allowed(u.Username); !ok {
		refuse(http.StatusTooManyRequests, rateLimitedMessage)
		return
	}

	// Mistakes in the new password are reported before the current one is checked, so
	// fixing a typo does not cost an attempt.
	if password != confirm {
		refuse(http.StatusBadRequest, "The two new passwords do not match.")
		return
	}
	if err := auth.ValidatePassword(password); err != nil {
		refuse(http.StatusBadRequest, capitalise(err.Error())+".")
		return
	}

	ok, err := auth.VerifyPassword(u.PasswordHash, current)
	if err != nil {
		s.log.ErrorContext(ctx, "stored password hash is unreadable", "user_id", u.ID, "error", err)
		refuse(http.StatusInternalServerError, "Something went wrong. Try again.")
		return
	}
	if !ok {
		s.loginByIP.RecordFailure(ipKey)
		s.loginByUser.RecordFailure(u.Username)
		s.audit.Record(ctx, audit.Entry{
			UserID: &u.ID, Action: audit.ActionPasswordChangeFailed,
			Entity: "user", EntityID: u.Username, IP: ip,
			Detail: map[string]any{"reason": "wrong_password"},
		})
		s.log.WarnContext(ctx, "password change refused", "user", u.Username,
			"reason", "wrong_password", "ip", ip)
		refuse(http.StatusBadRequest, "The current password is not correct.")
		return
	}

	if err := s.users.SetPassword(ctx, u.ID, password); err != nil {
		s.log.ErrorContext(ctx, "could not change password", "user_id", u.ID, "error", err)
		refuse(http.StatusInternalServerError, "Something went wrong. Try again.")
		return
	}
	s.loginByIP.Reset(ipKey)
	s.loginByUser.Reset(u.Username)

	// Every other browser signed in as this user is signed out, and this one carries on
	// under a fresh token.
	token, _, err := s.sessions.RotateAll(ctx, u.ID, r.UserAgent(), ip)
	if err != nil {
		s.log.ErrorContext(ctx, "could not rotate sessions after a password change",
			"user_id", u.ID, "error", err)
		refuse(http.StatusInternalServerError, "Your password was changed, but other browsers "+
			"could not be signed out. Sign out and back in to make sure.")
		return
	}
	auth.SetSessionCookie(w, token, s.cfg.CookieSecure, s.sessions.AbsoluteTTL())

	s.audit.Record(ctx, audit.Entry{
		UserID: &u.ID, Action: audit.ActionPasswordChanged,
		Entity: "user", EntityID: u.Username, IP: ip,
		Detail: map[string]any{"via": "web"},
	})
	s.log.InfoContext(ctx, "password changed", "user", u.Username, "ip", ip)

	http.Redirect(w, r, "/settings?msg="+url.QueryEscape(
		"Password changed. Every other browser and device has been signed out."), http.StatusSeeOther)
}

// capitalise upper-cases the first letter of a sentence, so a store error reads as
// one in the form.
func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
