package auth

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestLoadSessionPopulatesContext(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	userID := insertTestUser(t, d, "alice")

	sessions := NewSessionStore(d)
	token, _, err := sessions.Create(ctx, userID, "", "")
	if err != nil {
		t.Fatal(err)
	}

	var gotUser string
	handler := NewAuthenticator(sessions, false, discardLogger()).Load(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if u, ok := UserFromContext(r.Context()); ok {
				gotUser = u.Username
			}
			if _, ok := SessionFromContext(r.Context()); !ok {
				t.Error("the session is not in the context")
			}
		}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if gotUser != "alice" {
		t.Errorf("user in context = %q, want alice", gotUser)
	}
}

// TestLoadSessionClearsDeadCookie: a browser holding an expired cookie should
// stop sending it, rather than being told 401 forever.
func TestLoadSessionClearsDeadCookie(t *testing.T) {
	d := newTestDB(t)
	sessions := NewSessionStore(d)

	handler := NewAuthenticator(sessions, false, discardLogger()).Load(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := UserFromContext(r.Context()); ok {
				t.Error("an unknown token authenticated someone")
			}
		}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "a-token-that-does-not-exist"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("the dead session cookie was not cleared")
	}
}

func TestLoadSessionWithNoCookie(t *testing.T) {
	d := newTestDB(t)

	var reached bool
	handler := NewAuthenticator(NewSessionStore(d), false, discardLogger()).Load(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reached = true
			if _, ok := UserFromContext(r.Context()); ok {
				t.Error("a request with no cookie was authenticated")
			}
		}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if !reached {
		t.Error("Load blocked an anonymous request; that is RequireUser's job")
	}
}

// TestRequireUserResponseShape: HTML routes redirect, API routes get 401 JSON.
// An htmx or plugin client must not be handed a login page.
func TestRequireUserResponseShape(t *testing.T) {
	handler := RequireUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("RequireUser let an anonymous request through")
	}))

	t.Run("html route redirects", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

		if rec.Code != http.StatusSeeOther {
			t.Errorf("status = %d, want 303", rec.Code)
		}
		location := rec.Header().Get("Location")
		if !strings.HasPrefix(location, "/login") {
			t.Errorf("Location = %q, want a /login redirect", location)
		}
	})

	t.Run("api route returns json 401", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/healthz", nil))

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		if !strings.Contains(rec.Body.String(), "unauthorized") {
			t.Errorf("body = %q", rec.Body.String())
		}
	})
}

func TestRequireUserAllowsAuthenticated(t *testing.T) {
	var reached bool
	handler := RequireUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), userContextKey{}, User{ID: 1, Username: "alice"}))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if !reached {
		t.Error("an authenticated request was blocked")
	}
}

// TestSafeNextPath is the open-redirect defence. Without it,
// /login?next=https://evil.example borrows this app's credibility for a phishing
// page.
func TestSafeNextPath(t *testing.T) {
	tests := []struct {
		next string
		want string
	}{
		// Legitimate local targets survive untouched.
		{"/", "/"},
		{"/assets", "/assets"},
		{"/assets?q=sword", "/assets?q=sword"},
		// A fragment is never sent to the server, so dropping it loses nothing.
		{"/assets/42#panel", "/assets/42"},

		// Everything that could leave this origin collapses to "/".
		{"", "/"},
		{"https://evil.example", "/"},
		{"http://evil.example/path", "/"},
		{"//evil.example", "/"},
		{"//evil.example/path", "/"},
		{"///evil.example", "/"},
		{"javascript:alert(1)", "/"},
		{"data:text/html,<script>alert(1)</script>", "/"},
		{"\\\\evil.example", "/"},
		{"/\\evil.example", "/"},
		{"relative/path", "/"},
		{"../escape", "/"},

		// Header injection.
		{"/path\r\nSet-Cookie: a=b", "/"},
		{"/path\nX-Injected: 1", "/"},

		// Redirect loops.
		{"/login", "/"},
		{"/login?next=/login", "/"},
		{"/logout", "/"},
	}
	for _, tc := range tests {
		if got := SafeNextPath(tc.next); got != tc.want {
			t.Errorf("SafeNextPath(%q) = %q, want %q", tc.next, got, tc.want)
		}
	}
}
