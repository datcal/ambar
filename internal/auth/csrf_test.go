package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

var testSecret = []byte("a-test-secret-for-hmac-signing-only")

// newCSRFHandler wires Ensure and Protect the way the server does, and reports
// whether the inner handler was reached.
func newCSRFHandler(t *testing.T, secure bool) (http.Handler, *bool) {
	t.Helper()

	reached := new(bool)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*reached = true
		// Echo the token so tests can read what a page would have rendered.
		_, _ = w.Write([]byte(TokenFromContext(r.Context())))
	})
	c := NewCSRF(testSecret, secure)
	return c.Ensure(c.Protect(inner)), reached
}

func TestCSRFEnsureSetsCookieAndToken(t *testing.T) {
	handler, reached := newCSRFHandler(t, false)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))

	if !*reached {
		t.Fatal("a GET did not reach the handler")
	}

	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == CSRFCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no CSRF cookie was set")
	}
	if !cookie.HttpOnly {
		t.Error("the CSRF cookie is not HttpOnly")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", cookie.SameSite)
	}
	if token := rec.Body.String(); token == "" {
		t.Error("no token was put in the request context")
	} else if token == cookie.Value {
		t.Error("the token is the raw cookie value; it must be the HMAC of it")
	}
}

// TestCSRFCookieSecureFollowsConfig ties the cookie to the same resolved setting
// as the session cookie, so a plain-HTTP LAN deployment is not half-broken.
func TestCSRFCookieSecureFollowsConfig(t *testing.T) {
	for _, secure := range []bool{true, false} {
		handler, _ := newCSRFHandler(t, secure)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))

		for _, c := range rec.Result().Cookies() {
			if c.Name == CSRFCookieName && c.Secure != secure {
				t.Errorf("cookie Secure = %v, want %v", c.Secure, secure)
			}
		}
	}
}

// issueToken performs the GET a browser would do first, returning the cookie and
// the matching token.
func issueToken(t *testing.T, handler http.Handler) (*http.Cookie, string) {
	t.Helper()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))

	for _, c := range rec.Result().Cookies() {
		if c.Name == CSRFCookieName {
			return c, rec.Body.String()
		}
	}
	t.Fatal("no CSRF cookie issued")
	return nil, ""
}

func TestCSRFProtectAcceptsValidToken(t *testing.T) {
	handler, reached := newCSRFHandler(t, false)
	cookie, token := issueToken(t, handler)

	t.Run("form field", func(t *testing.T) {
		*reached = false
		body := url.Values{CSRFFieldName: {token}, "username": {"alice"}}.Encode()
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK || !*reached {
			t.Errorf("valid form token rejected: status %d", rec.Code)
		}
	})

	t.Run("header", func(t *testing.T) {
		*reached = false
		req := httptest.NewRequest(http.MethodPost, "/login", nil)
		req.Header.Set(CSRFHeaderName, token)
		req.AddCookie(cookie)

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK || !*reached {
			t.Errorf("valid header token rejected: status %d", rec.Code)
		}
	})
}

// TestCSRFProtectRejects is the core of the defence. Every case must 403 and
// must not reach the handler.
func TestCSRFProtectRejects(t *testing.T) {
	handler, reached := newCSRFHandler(t, false)
	goodCookie, goodToken := issueToken(t, handler)

	// A second, independent visitor: their token must not work with the first
	// visitor's cookie.
	otherCookie, otherToken := issueToken(t, handler)
	if otherCookie.Value == goodCookie.Value {
		t.Fatal("two visitors were issued the same CSRF cookie")
	}

	tests := []struct {
		name   string
		cookie *http.Cookie
		token  string
	}{
		{"no cookie, no token", nil, ""},
		{"no cookie, valid-looking token", nil, goodToken},
		{"cookie but no token", goodCookie, ""},
		{"cookie with wrong token", goodCookie, "not-the-right-token"},
		{"cookie with another visitor's token", goodCookie, otherToken},
		{"empty cookie value", &http.Cookie{Name: CSRFCookieName, Value: ""}, goodToken},
		{"attacker-chosen cookie", &http.Cookie{Name: CSRFCookieName, Value: "chosen"}, goodToken},
		{"token truncated", goodCookie, goodToken[:len(goodToken)-1]},
		{"token with trailing space", goodCookie, goodToken + " "},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			*reached = false
			req := httptest.NewRequest(http.MethodPost, "/login", nil)
			if tc.cookie != nil {
				req.AddCookie(tc.cookie)
			}
			if tc.token != "" {
				req.Header.Set(CSRFHeaderName, tc.token)
			}

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rec.Code)
			}
			if *reached {
				t.Error("the request reached the handler despite a failed CSRF check")
			}
		})
	}
}

// TestCSRFUnsafeMethodsAreAllChecked: a state-changing DELETE must not slip past
// because only POST was considered.
func TestCSRFUnsafeMethodsAreAllChecked(t *testing.T) {
	handler, reached := newCSRFHandler(t, false)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		*reached = false
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(method, "/thing", nil))

		if rec.Code != http.StatusForbidden {
			t.Errorf("%s without a token got %d, want 403", method, rec.Code)
		}
		if *reached {
			t.Errorf("%s without a token reached the handler", method)
		}
	}
}

func TestCSRFSafeMethodsPassThrough(t *testing.T) {
	handler, reached := newCSRFHandler(t, false)

	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		*reached = false
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(method, "/thing", nil))

		if !*reached {
			t.Errorf("%s was blocked", method)
		}
	}
}

// TestCSRFRejectionShapeMatchesRoute: the API gets JSON, the UI gets prose.
func TestCSRFRejectionShapeMatchesRoute(t *testing.T) {
	handler, _ := newCSRFHandler(t, false)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/things", nil))
	if !strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("an /api/ rejection is not JSON: %q", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/logout", nil))
	if strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("a UI rejection returned JSON: %q", rec.Body.String())
	}
}

// TestCSRFFormBodyStillReadableByHandler guards a subtle trap: the middleware
// calls ParseForm to find the token, and the handler must still see the fields.
func TestCSRFFormBodyStillReadableByHandler(t *testing.T) {
	c := NewCSRF(testSecret, false)
	var gotUsername string
	handler := c.Ensure(c.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUsername = r.PostFormValue("username")
	})))

	// Mint a cookie and token through the same handler.
	probe := c.Ensure(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(TokenFromContext(r.Context())))
	}))
	rec := httptest.NewRecorder()
	probe.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	token := rec.Body.String()
	cookie := rec.Result().Cookies()[0]

	body := url.Values{CSRFFieldName: {token}, "username": {"alice"}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)

	handler.ServeHTTP(httptest.NewRecorder(), req)

	if gotUsername != "alice" {
		t.Errorf("handler read username = %q; the middleware consumed the body", gotUsername)
	}
}

// TestCSRFMultipartRequiresHeader documents the deliberate choice not to parse
// multipart bodies in the middleware — that would buffer an entire M4 archive
// upload before any size cap applied.
func TestCSRFMultipartRequiresHeader(t *testing.T) {
	handler, reached := newCSRFHandler(t, false)
	cookie, token := issueToken(t, handler)

	t.Run("field only is rejected", func(t *testing.T) {
		*reached = false
		req := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader("irrelevant"))
		req.Header.Set("Content-Type", "multipart/form-data; boundary=xyz")
		req.AddCookie(cookie)

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("header is accepted", func(t *testing.T) {
		*reached = false
		req := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader("irrelevant"))
		req.Header.Set("Content-Type", "multipart/form-data; boundary=xyz")
		req.Header.Set(CSRFHeaderName, token)
		req.AddCookie(cookie)

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if !*reached {
			t.Errorf("a multipart POST with a valid header token was rejected: %d", rec.Code)
		}
	})
}

// TestCSRFTokenDependsOnTheSecret: without the server secret, an attacker who
// somehow learns the cookie still cannot compute the token.
func TestCSRFTokenDependsOnTheSecret(t *testing.T) {
	a := NewCSRF([]byte("secret-a"), false)
	b := NewCSRF([]byte("secret-b"), false)

	if a.sign("same-id") == b.sign("same-id") {
		t.Error("the same ID signs to the same token under different secrets")
	}
	if a.sign("id-1") == a.sign("id-2") {
		t.Error("different IDs sign to the same token")
	}
}

func TestTokenFromContextWithoutMiddleware(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := TokenFromContext(req.Context()); got != "" {
		t.Errorf("TokenFromContext = %q on a bare request, want empty", got)
	}
}
