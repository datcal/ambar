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
// Deliberately not offered: deleting a user or changing someone else's password.
// A deleted user's sessions, audit entries and manual tags all reference them, and
// §11's "two equal users" gives nobody the authority to lock the other out. Removing
// an account stays a CLI decision made on purpose.

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w, r, http.StatusOK, "", r.URL.Query().Get("msg"))
}

// renderSettings draws the page with an optional error and flash.
func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, status int, formError, flash string) {
	data := s.newPageData(r)
	data.Nav = "settings"
	data.UserError = formError
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
		s.renderSettings(w, r, http.StatusBadRequest, "Could not read the form.", "")
		return
	}
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	confirm := r.PostFormValue("password_confirm")

	switch {
	case username == "":
		s.renderSettings(w, r, http.StatusBadRequest, "A username is required.", "")
		return
	case password != confirm:
		// Checked before the store so the message is about the mistake actually made.
		s.renderSettings(w, r, http.StatusBadRequest, "The two passwords do not match.", "")
		return
	}

	user, err := s.users.Create(r.Context(), username, password, auth.RoleUser)
	if err != nil {
		// The store's errors are already written for a human ("username is taken",
		// "password must be at least N characters"), so they are shown as they are.
		s.log.InfoContext(r.Context(), "user creation refused", "username", username, "error", err)
		s.renderSettings(w, r, http.StatusBadRequest, capitalise(err.Error())+".", "")
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

// capitalise upper-cases the first letter of a sentence, so a store error reads as
// one in the form.
func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
