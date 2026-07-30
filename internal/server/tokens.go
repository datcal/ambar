package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/datcal/ambar/internal/audit"
	"github.com/datcal/ambar/internal/auth"
)

// handleTokensPage lists the current user's API tokens (§11).
func (s *Server) handleTokensPage(w http.ResponseWriter, r *http.Request) {
	s.renderTokens(w, r, "", http.StatusOK)
}

// handleTokenCreate mints a token and shows the plaintext exactly once (§11).
func (s *Server) handleTokenCreate(w http.ResponseWriter, r *http.Request) {
	u, ok := auth.UserFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	name := r.PostFormValue("name")
	var scopes []string
	for _, sc := range r.PostForm["scope"] {
		scopes = append(scopes, sc)
	}
	var expires *time.Time
	if t, ok := parseDate(r.PostFormValue("expires")); ok {
		expires = &t
	}

	plain, _, err := s.tokens.Create(r.Context(), u.ID, name, scopes, expires)
	if err != nil {
		s.renderTokensError(w, r, "Could not create the token: "+err.Error())
		return
	}
	s.audit.Record(r.Context(), audit.Entry{
		UserID: &u.ID, Action: "token.created", Entity: "api_token",
		Detail: map[string]any{"name": name},
	})
	// Render the page with the plaintext shown once — never redirected (a token in
	// a URL would land in logs and history).
	s.renderTokens(w, r, plain, http.StatusCreated)
}

// handleTokenRevoke revokes one of the user's tokens.
func (s *Server) handleTokenRevoke(w http.ResponseWriter, r *http.Request) {
	u, ok := auth.UserFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err := s.tokens.Revoke(r.Context(), id, u.ID); err != nil {
		s.log.ErrorContext(r.Context(), "revoke token failed", "token_id", id, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	s.audit.Record(r.Context(), audit.Entry{
		UserID: &u.ID, Action: "token.revoked", Entity: "api_token",
		EntityID: strconv.FormatInt(id, 10),
	})
	http.Redirect(w, r, "/settings/tokens", http.StatusSeeOther)
}

func (s *Server) renderTokens(w http.ResponseWriter, r *http.Request, plaintext string, status int) {
	u, ok := auth.UserFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	tokens, err := s.tokens.List(r.Context(), u.ID)
	if err != nil {
		s.log.ErrorContext(r.Context(), "listing tokens failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	data := s.newPageData(r)
	data.Tokens = tokens
	data.NewToken = plaintext
	s.render(w, r, "tokens.html", status, data)
}

func (s *Server) renderTokensError(w http.ResponseWriter, r *http.Request, msg string) {
	data := s.newPageData(r)
	if u, ok := auth.UserFromContext(r.Context()); ok {
		if tokens, err := s.tokens.List(r.Context(), u.ID); err == nil {
			data.Tokens = tokens
		}
	}
	data.TokenError = msg
	s.render(w, r, "tokens.html", http.StatusUnprocessableEntity, data)
}
