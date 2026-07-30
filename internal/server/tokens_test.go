package server

import (
	"net/url"
	"strings"
	"testing"
)

func TestTokenManagementUI(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	// Create a token: the plaintext is shown once on the response.
	status, body := ts.postForm(t, "/settings/tokens", url.Values{
		"name": {"laptop"}, "scope": {"write"},
	})
	if status != 201 && status != 200 {
		t.Fatalf("create status = %d", status)
	}
	if !strings.Contains(body, "ambar_") {
		t.Fatalf("plaintext token not shown on creation")
	}

	// The list page shows the token by name but never the secret.
	list := readBody(t, ts.get(t, "/settings/tokens"))
	if !strings.Contains(list, "laptop") {
		t.Errorf("token not listed")
	}
	if strings.Contains(list, "ambar_") {
		t.Errorf("the list page leaked a token secret")
	}

	// Revoke it.
	var id int64
	if err := ts.db.Reader.QueryRow(`SELECT id FROM api_tokens WHERE name = 'laptop'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	status, _ = ts.postForm(t, itoa("/settings/tokens/%d/revoke", id), url.Values{})
	if status != 200 && status != 303 {
		t.Fatalf("revoke status = %d", status)
	}
	var revoked *int64
	ts.db.Reader.QueryRow(`SELECT revoked_at FROM api_tokens WHERE id = ?`, id).Scan(&revoked)
	if revoked == nil {
		t.Errorf("token not revoked")
	}
}
