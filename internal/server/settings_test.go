package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
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
