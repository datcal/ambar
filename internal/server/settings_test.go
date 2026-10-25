package server

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"

	"github.com/datcal/ambar/internal/auth"
)

// TestSettingsCreatesUsers covers M15's answer to "adding a user must be possible":
// §11 forbids self-registration, so an account is created by someone already signed
// in, and the creation is audited.
func TestSettingsCreatesUsers(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	body := ts.body(t, ts.get(t, "/settings"))
	for _, want := range []string{"Users", testUsername, "Add a user", "no self-registration"} {
		if !strings.Contains(body, want) {
			t.Errorf("the settings page is missing %q", want)
		}
	}

	// Create one.
	status, body := ts.postForm(t, "/settings/users", url.Values{
		"username":         {"partner"},
		"password":         {"another-long-enough-password"},
		"password_confirm": {"another-long-enough-password"},
	})
	if status != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303\n%s", status, body)
	}
	if body := ts.body(t, ts.get(t, "/settings")); !strings.Contains(body, "partner") {
		t.Error("the new user is not listed")
	}

	// It is audited, with who did it.
	var n int
	if err := ts.db.Reader.QueryRow(`
		SELECT count(*) FROM audit_log
		WHERE action = 'user.created' AND entity_id = 'partner' AND user_id IS NOT NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("audit rows for the creation = %d, want 1", n)
	}

	// And the new account can sign in.
	fresh := ts
	resp := fresh.login(t, "partner", "another-long-enough-password")
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("the created user cannot sign in: status %d", resp.StatusCode)
	}
}

// TestSettingsRefusesBadUsers: the form reports the mistake actually made rather than
// a generic failure, and creates nothing.
func TestSettingsRefusesBadUsers(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	cases := []struct {
		name string
		form url.Values
		want string
	}{
		{
			name: "mismatched passwords",
			form: url.Values{"username": {"partner"}, "password": {"a-long-enough-password"},
				"password_confirm": {"a-different-password"}},
			want: "do not match",
		},
		{
			name: "short password",
			form: url.Values{"username": {"partner"}, "password": {"short"},
				"password_confirm": {"short"}},
			want: "characters",
		},
		{
			name: "no username",
			form: url.Values{"username": {""}, "password": {"a-long-enough-password"},
				"password_confirm": {"a-long-enough-password"}},
			want: "username is required",
		},
		{
			name: "duplicate username",
			form: url.Values{"username": {testUsername}, "password": {"a-long-enough-password"},
				"password_confirm": {"a-long-enough-password"}},
			want: "taken",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := ts.postForm(t, "/settings/users", tc.form)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", status)
			}
			if !strings.Contains(strings.ToLower(body), strings.ToLower(tc.want)) {
				t.Errorf("the error does not mention %q:\n%s", tc.want, body)
			}
		})
	}

	var n int
	if err := ts.db.Reader.QueryRow(`SELECT count(*) FROM users`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d users exist; none of the refused forms may have created one", n)
	}
}

// secondBrowser is another client of the same server with a cookie jar of its own:
// the same person signed in on a second device.
func (ts *testServer) secondBrowser(t *testing.T) *testServer {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	other := *ts
	other.client = &http.Client{Jar: jar, CheckRedirect: ts.client.CheckRedirect}
	return &other
}

// passwordVerifies reports whether password is the one stored for the test user.
func (ts *testServer) passwordVerifies(t *testing.T, password string) bool {
	t.Helper()
	u, err := auth.NewUserStore(ts.db).ByUsername(context.Background(), testUsername)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := auth.VerifyPassword(u.PasswordHash, password)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// TestSettingsChangesOwnPassword: the change takes the current password, signs out
// every other browser, keeps this one signed in under a new token, and is audited.
func TestSettingsChangesOwnPassword(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	other := ts.secondBrowser(t)
	other.login(t, testUsername, testPassword)
	before := ts.sessionCookie(t)

	if body := ts.body(t, ts.get(t, "/settings")); !strings.Contains(body, "Your password") {
		t.Fatal("the settings page has no password form")
	}

	const newPassword = "a-brand-new-test-password"
	status, body := ts.postForm(t, "/settings/password", url.Values{
		"current_password": {testPassword},
		"password":         {newPassword},
		"password_confirm": {newPassword},
	})
	if status != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303\n%s", status, body)
	}

	if after := ts.sessionCookie(t); after == nil || after.Value == before.Value {
		t.Error("the session token was not rotated by the password change")
	}
	if resp := ts.get(t, "/settings"); resp.StatusCode != http.StatusOK {
		t.Errorf("the browser that changed the password was signed out: status %d", resp.StatusCode)
	}
	resp := other.get(t, "/settings")
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login") {
		t.Errorf("the other browser is still signed in: status %d", resp.StatusCode)
	}

	if !ts.passwordVerifies(t, newPassword) || ts.passwordVerifies(t, testPassword) {
		t.Error("the stored password is not the new one")
	}
	fresh := ts.secondBrowser(t)
	if resp := fresh.login(t, testUsername, newPassword); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("the new password does not sign in: status %d", resp.StatusCode)
	}

	var n int
	if err := ts.db.Reader.QueryRow(`
		SELECT count(*) FROM audit_log
		WHERE action = 'password.changed' AND entity_id = ? AND user_id IS NOT NULL`,
		testUsername).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("audit rows for the change = %d, want 1", n)
	}
}

// TestSettingsRefusesBadPasswordChanges: each mistake gets its own message, and none
// of them changes the password or signs anybody out.
func TestSettingsRefusesBadPasswordChanges(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	other := ts.secondBrowser(t)
	other.login(t, testUsername, testPassword)

	cases := []struct {
		name string
		form url.Values
		want string
	}{
		{
			name: "wrong current password",
			form: url.Values{"current_password": {"not-the-password-at-all"},
				"password": {"a-brand-new-test-password"}, "password_confirm": {"a-brand-new-test-password"}},
			want: "current password is not correct",
		},
		{
			name: "mismatched new passwords",
			form: url.Values{"current_password": {testPassword},
				"password": {"a-brand-new-test-password"}, "password_confirm": {"a-different-new-password"}},
			want: "do not match",
		},
		{
			name: "short new password",
			form: url.Values{"current_password": {testPassword},
				"password": {"short"}, "password_confirm": {"short"}},
			want: "at least",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := ts.postForm(t, "/settings/password", tc.form)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", status)
			}
			if !strings.Contains(strings.ToLower(body), tc.want) {
				t.Errorf("the error does not mention %q:\n%s", tc.want, body)
			}
		})
	}

	if !ts.passwordVerifies(t, testPassword) {
		t.Error("a refused change still replaced the password")
	}
	if resp := other.get(t, "/settings"); resp.StatusCode != http.StatusOK {
		t.Errorf("a refused change signed the other browser out: status %d", resp.StatusCode)
	}

	// Only the wrong current password is worth an audit entry; the others are typos.
	var n int
	if err := ts.db.Reader.QueryRow(`
		SELECT count(*) FROM audit_log WHERE action = 'password.change_failed'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("audit rows for refused changes = %d, want 1", n)
	}
}

// TestPasswordChangeIsRateLimited: guessing the current password through the form
// runs into the same per-username limit as /login, so a borrowed session cannot be
// turned into the password by brute force.
func TestPasswordChangeIsRateLimited(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	const newPassword = "a-brand-new-test-password"
	for i := 0; i < auth.LoginAttemptsPerUsername; i++ {
		status, _ := ts.postForm(t, "/settings/password", url.Values{
			"current_password": {"wrong-guess-number-" + string(rune('a'+i))},
			"password":         {newPassword}, "password_confirm": {newPassword},
		})
		if status != http.StatusBadRequest {
			t.Fatalf("guess %d: status = %d, want 400", i+1, status)
		}
	}

	// Now even the right password is turned away until the window passes.
	status, body := ts.postForm(t, "/settings/password", url.Values{
		"current_password": {testPassword},
		"password":         {newPassword}, "password_confirm": {newPassword},
	})
	if status != http.StatusTooManyRequests {
		t.Fatalf("status after %d wrong guesses = %d, want 429", auth.LoginAttemptsPerUsername, status)
	}
	if !strings.Contains(body, rateLimitedMessage) {
		t.Errorf("the refusal does not say why:\n%s", body)
	}
	if !ts.passwordVerifies(t, testPassword) {
		t.Error("the password changed despite the rate limit")
	}
}
