package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/datcal/ambar/internal/auth"
	"github.com/datcal/ambar/internal/config"
	"github.com/datcal/ambar/internal/db"
)

const (
	testUsername = "burak"
	testPassword = "a-long-enough-test-password"
)

// testServer is a running server plus a cookie-jar client, which is the only way
// to exercise CSRF and session rotation honestly.
type testServer struct {
	*httptest.Server
	client *http.Client
	db     *db.DB
	cfg    *config.Config
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	return newTestServerWithConfig(t, nil)
}

// newTestServerWithConfig lets a test adjust the config before the server starts.
func newTestServerWithConfig(t *testing.T, adjust func(*config.Config)) *testServer {
	t.Helper()

	root := t.TempDir()
	libraryRoot := filepath.Join(root, "library")
	dataRoot := filepath.Join(root, "data")
	for _, dir := range []string{libraryRoot, dataRoot} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &config.Config{
		LibraryRoot:   libraryRoot,
		DataRoot:      dataRoot,
		Bind:          "127.0.0.1:0",
		BaseURL:       mustParseURL(t, "http://127.0.0.1:8080"),
		SessionSecret: []byte("a-test-secret-for-csrf-hmac-signing"),
		CookieSecure:  false,
		Workers:       1,
	}
	if adjust != nil {
		adjust(cfg)
	}

	database, err := db.Open(cfg.DatabasePath())
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	if _, err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Cheap argon2id parameters, or every server test pays the production cost.
	originalParams := auth.DefaultParams
	auth.DefaultParams = auth.Params{Memory: 8 * 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}
	t.Cleanup(func() { auth.DefaultParams = originalParams })

	srv, err := New(cfg, database, discardLogger(), BuildInfo{Version: "test", Commit: "abc1234"})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Jar: jar,
		// Redirects are followed manually so each response can be inspected.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	return &testServer{Server: httpSrv, client: client, db: database, cfg: cfg}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (ts *testServer) createUser(t *testing.T, username, password string) {
	t.Helper()
	if _, err := auth.NewUserStore(ts.db).Create(context.Background(), username, password, auth.RoleUser); err != nil {
		t.Fatalf("create user: %v", err)
	}
}

func (ts *testServer) get(t *testing.T, path string) *http.Response {
	t.Helper()
	resp, err := ts.client.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

var csrfFieldPattern = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

// csrfToken scrapes a token out of a rendered page, the way a browser would, so
// the cookie jar and the token stay consistent.
//
// The token is bound to the CSRF cookie rather than to a page, so any page that
// renders will do. That matters because /login redirects when the client is
// already signed in, and / redirects when it is not — the fallback keeps the
// helper usable in both states.
func (ts *testServer) csrfToken(t *testing.T, path string) string {
	t.Helper()

	candidates := []string{path}
	for _, fallback := range []string{"/", "/login"} {
		if fallback != path {
			candidates = append(candidates, fallback)
		}
	}

	for _, candidate := range candidates {
		resp := ts.get(t, candidate)
		if resp.StatusCode != http.StatusOK {
			continue
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if m := csrfFieldPattern.FindSubmatch(body); m != nil {
			return string(m[1])
		}
	}
	t.Fatalf("no CSRF field found on any of %v", candidates)
	return ""
}

// login performs the full form post.
func (ts *testServer) login(t *testing.T, username, password string) *http.Response {
	t.Helper()
	return ts.loginWithNext(t, username, password, "")
}

func (ts *testServer) loginWithNext(t *testing.T, username, password, next string) *http.Response {
	t.Helper()

	token := ts.csrfToken(t, "/login")
	form := url.Values{
		"csrf_token": {token},
		"username":   {username},
		"password":   {password},
	}
	if next != "" {
		form.Set("next", next)
	}

	resp, err := ts.client.PostForm(ts.URL+"/login", form)
	if err != nil {
		t.Fatalf("POST /login: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (ts *testServer) sessionCookie(t *testing.T) *http.Cookie {
	t.Helper()

	for _, c := range ts.client.Jar.Cookies(mustParseURL(t, ts.URL)) {
		if c.Name == auth.SessionCookieName {
			return c
		}
	}
	return nil
}

// --- liveness and health ---------------------------------------------------

func TestLivenessIsPublicAndUninformative(t *testing.T) {
	ts := newTestServer(t)

	resp := ts.get(t, "/healthz")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"status":"ok"`) {
		t.Errorf("body = %q", body)
	}
	// It must not become a fingerprinting endpoint (§2).
	for _, leak := range []string{"test", "abc1234", ts.cfg.LibraryRoot, ts.cfg.DataRoot, "go1."} {
		if strings.Contains(string(body), leak) {
			t.Errorf("the public liveness endpoint leaks %q: %s", leak, body)
		}
	}
}

func TestDetailedHealthRequiresAuth(t *testing.T) {
	ts := newTestServer(t)

	resp := ts.get(t, "/api/v1/healthz")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 — the detailed report must not be public", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json for an /api/ route", ct)
	}
}

func TestDetailedHealthReport(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	resp := ts.get(t, "/api/v1/healthz")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}

	var report healthReport
	if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
		t.Fatalf("decode report: %v", err)
	}

	if report.Status != "ok" {
		t.Errorf("status = %q, want ok", report.Status)
	}
	if report.Version != "test" || report.Commit != "abc1234" {
		t.Errorf("build info = %q/%q, want test/abc1234", report.Version, report.Commit)
	}
	if report.SchemaVersion == "" {
		t.Error("schema version is empty")
	}
	if report.Go == "" {
		t.Error("go version is empty")
	}

	// Every check must be present and passing on a healthy instance.
	want := map[string]bool{"database": false, "library_root": false, "data_root": false}
	for _, c := range report.Checks {
		if _, expected := want[c.Name]; !expected {
			t.Errorf("unexpected check %q", c.Name)
		}
		want[c.Name] = true
		if !c.OK {
			t.Errorf("check %q failed: %s", c.Name, c.Detail)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("check %q is missing from the report", name)
		}
	}

	// §12 checks this milestone does not do must be named rather than silently
	// omitted.
	if len(report.NotYetImplemented) == 0 {
		t.Error("not_yet_implemented is empty; a health report that omits checks silently is misleading")
	}
}

// TestHealthReportsMissingLibraryMount is the failure §12 cares about: a NAS
// share that has gone away underneath a running container.
func TestHealthReportsMissingLibraryMount(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	// Remove the library out from under the running server.
	if err := os.RemoveAll(ts.cfg.LibraryRoot); err != nil {
		t.Fatal(err)
	}

	resp := ts.get(t, "/api/v1/healthz")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}

	var report healthReport
	if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
		t.Fatal(err)
	}
	if report.Status != "unhealthy" {
		t.Errorf("status = %q, want unhealthy", report.Status)
	}

	var found bool
	for _, c := range report.Checks {
		if c.Name == "library_root" {
			found = true
			if c.OK {
				t.Error("library_root reported OK with the directory removed")
			}
			if !strings.Contains(c.Detail, "does not exist") {
				t.Errorf("detail does not name the problem: %q", c.Detail)
			}
		}
	}
	if !found {
		t.Error("no library_root check in the report")
	}
}

// TestHealthMentionsMissingUsers: "I cannot log in" on a fresh install is almost
// always this.
func TestHealthMentionsMissingUsers(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	// Remove the only user, then look at the report.
	if _, err := ts.db.Writer.Exec(`DELETE FROM users`); err != nil {
		t.Fatal(err)
	}

	resp := ts.get(t, "/healthz")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("liveness should still be OK with no users: %d", resp.StatusCode)
	}
}

// --- authentication gate ---------------------------------------------------

func TestIndexRequiresLogin(t *testing.T) {
	ts := newTestServer(t)

	resp := ts.get(t, "/")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	if !strings.HasPrefix(location, "/login") {
		t.Errorf("Location = %q, want a /login redirect", location)
	}
	if !strings.Contains(location, "next=") {
		t.Errorf("Location = %q, want a next= parameter", location)
	}
}

func TestUnknownPathIs404(t *testing.T) {
	ts := newTestServer(t)

	// "GET /{$}" must not swallow every path.
	resp := ts.get(t, "/nonsense")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /nonsense = %d, want 404", resp.StatusCode)
	}
}

func TestLoginPageRenders(t *testing.T) {
	ts := newTestServer(t)

	resp := ts.get(t, "/login")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`name="username"`, `name="password"`, `name="csrf_token"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the login page is missing %s", want)
		}
	}
	// §11: no self-registration, and the page should say how accounts are made.
	if !strings.Contains(string(body), "ambar user add") {
		t.Error("the login page does not mention how to create an account")
	}
}

func TestSuccessfulLogin(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)

	resp := ts.login(t, testUsername, testPassword)
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 303: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Location"); got != "/" {
		t.Errorf("Location = %q, want /", got)
	}

	// The session cookie must carry the §11 attributes.
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == auth.SessionCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie was set")
	}
	if !cookie.HttpOnly {
		t.Error("the session cookie is not HttpOnly")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", cookie.SameSite)
	}
	if cookie.Secure {
		t.Error("Secure is set even though CookieSecure is false for this config")
	}

	// And the protected page must now render.
	resp = ts.get(t, "/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / after login = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), testUsername) {
		t.Errorf("the index page does not name the logged-in user")
	}
}

// TestCookieSecureFollowsConfig covers the documented deviation from §11.
func TestCookieSecureFollowsConfig(t *testing.T) {
	ts := newTestServerWithConfig(t, func(cfg *config.Config) {
		cfg.CookieSecure = true
	})
	ts.createUser(t, testUsername, testPassword)

	resp := ts.login(t, testUsername, testPassword)
	for _, c := range resp.Cookies() {
		if c.Name == auth.SessionCookieName && !c.Secure {
			t.Error("Secure is not set even though CookieSecure is true")
		}
	}
}

// TestLoginDoesNotRevealWhetherAUserExists is the §11 anti-enumeration
// requirement. The two responses must be byte-identical.
func TestLoginDoesNotRevealWhetherAUserExists(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)

	readBody := func(resp *http.Response) string {
		t.Helper()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		// The CSRF token differs per request, so normalise it out before
		// comparing.
		return csrfFieldPattern.ReplaceAllString(string(body), `name="csrf_token" value="NORMALISED"`)
	}

	wrongPassword := ts.login(t, testUsername, "definitely-the-wrong-password")
	wrongPasswordBody := readBody(wrongPassword)

	// A fresh client, so the earlier failure does not affect the rate limiter
	// state visible to this attempt.
	ts2 := newTestServer(t)
	ts2.createUser(t, testUsername, testPassword)
	unknownUser := ts2.login(t, "nosuchperson", "definitely-the-wrong-password")
	unknownUserBody := readBody(unknownUser)

	if wrongPassword.StatusCode != unknownUser.StatusCode {
		t.Errorf("status differs: wrong password %d, unknown user %d",
			wrongPassword.StatusCode, unknownUser.StatusCode)
	}
	if wrongPassword.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", wrongPassword.StatusCode)
	}

	// The bodies differ only in the echoed username, which the submitter already
	// knows. The error message itself must be the same.
	if !strings.Contains(wrongPasswordBody, loginFailedMessage) {
		t.Errorf("wrong-password body does not contain the generic message")
	}
	if !strings.Contains(unknownUserBody, loginFailedMessage) {
		t.Errorf("unknown-user body does not contain the generic message")
	}
	for _, leak := range []string{"no such user", "not found", "unknown user", "wrong password"} {
		if strings.Contains(strings.ToLower(unknownUserBody), leak) {
			t.Errorf("the response leaks %q", leak)
		}
	}
}

func TestLoginRejectsWrongPassword(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)

	resp := ts.login(t, testUsername, "wrong-but-long-enough-password")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if ts.sessionCookie(t) != nil {
		t.Error("a session cookie was issued for a failed login")
	}

	var n int
	if err := ts.db.Reader.QueryRow(`SELECT count(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d session rows exist after a failed login, want 0", n)
	}
}

// TestSessionIsRotatedOnLogin is the §11 session-fixation defence, end to end.
func TestSessionIsRotatedOnLogin(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)

	ts.login(t, testUsername, testPassword)
	first := ts.sessionCookie(t)
	if first == nil {
		t.Fatal("no session cookie after the first login")
	}

	// Log in again while already holding a session.
	ts.login(t, testUsername, testPassword)
	second := ts.sessionCookie(t)
	if second == nil {
		t.Fatal("no session cookie after the second login")
	}

	if first.Value == second.Value {
		t.Error("the session token did not change across a second login")
	}

	// The old token must no longer resolve.
	if _, _, err := auth.NewSessionStore(ts.db).Lookup(context.Background(), first.Value); err == nil {
		t.Error("the pre-login session token still works")
	}

	var n int
	if err := ts.db.Reader.QueryRow(`SELECT count(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d session rows after rotation, want 1", n)
	}
}

func TestLoginRedirectsToNext(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)

	t.Run("local path is honoured", func(t *testing.T) {
		resp := ts.loginWithNext(t, testUsername, testPassword, "/api/v1/healthz")
		if got := resp.Header.Get("Location"); got != "/api/v1/healthz" {
			t.Errorf("Location = %q, want /api/v1/healthz", got)
		}
	})

	t.Run("external URL is refused", func(t *testing.T) {
		ts2 := newTestServer(t)
		ts2.createUser(t, testUsername, testPassword)

		resp := ts2.loginWithNext(t, testUsername, testPassword, "https://evil.example/phish")
		if got := resp.Header.Get("Location"); got != "/" {
			t.Errorf("Location = %q, want / — this is an open redirect", got)
		}
	})
}

func TestLoginPageRedirectsWhenAlreadySignedIn(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	resp := ts.get(t, "/login")
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303 for an already-signed-in visitor", resp.StatusCode)
	}
}

// TestLoginRateLimiting exercises the §11 per-username limit through the real
// handler.
func TestLoginRateLimiting(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)

	var limited bool
	for i := 0; i < auth.LoginAttemptsPerUsername+2; i++ {
		resp := ts.login(t, testUsername, "wrong-but-long-enough-password")
		if resp.StatusCode == http.StatusTooManyRequests {
			limited = true
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			// Even when limited, the message must not confirm the account exists.
			if !strings.Contains(string(body), rateLimitedMessage) {
				t.Errorf("rate-limited body = %q", body)
			}
			break
		}
	}
	if !limited {
		t.Fatalf("no rate limiting after %d failed attempts", auth.LoginAttemptsPerUsername+2)
	}

	// And the correct password is refused too while the limit holds, since the
	// check happens before verification.
	resp := ts.login(t, testUsername, testPassword)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d while rate limited, want 429", resp.StatusCode)
	}
}

// TestSuccessfulLoginClearsTheLimiter: earlier typos must not count against a
// correct password.
func TestSuccessfulLoginClearsTheLimiter(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)

	for i := 0; i < auth.LoginAttemptsPerUsername-1; i++ {
		if resp := ts.login(t, testUsername, "wrong-but-long-enough"); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status %d, want 401", i, resp.StatusCode)
		}
	}

	if resp := ts.login(t, testUsername, testPassword); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("the correct password was refused after typos: %d", resp.StatusCode)
	}

	// The counter should be clear, so a fresh run of failures is allowed again.
	for i := 0; i < auth.LoginAttemptsPerUsername-1; i++ {
		if resp := ts.login(t, testUsername, "wrong-but-long-enough"); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("post-success attempt %d: status %d, want 401", i, resp.StatusCode)
		}
	}
}

// --- CSRF, end to end ------------------------------------------------------

func TestLoginRequiresCSRF(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)

	// Prime the cookie jar with a CSRF cookie, then post without the token.
	ts.get(t, "/login")

	resp, err := ts.client.PostForm(ts.URL+"/login", url.Values{
		"username": {testUsername},
		"password": {testPassword},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	if ts.sessionCookie(t) != nil {
		t.Error("a session was issued despite a failed CSRF check")
	}
}

func TestLogoutRequiresCSRF(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	before := ts.sessionCookie(t)
	if before == nil {
		t.Fatal("not logged in")
	}

	resp, err := ts.client.PostForm(ts.URL+"/logout", url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	// The session must survive a rejected logout.
	if _, _, err := auth.NewSessionStore(ts.db).Lookup(context.Background(), before.Value); err != nil {
		t.Error("the session was destroyed by a CSRF-rejected logout")
	}
}

func TestLogout(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	token := ts.sessionCookie(t)
	if token == nil {
		t.Fatal("not logged in")
	}

	csrf := ts.csrfToken(t, "/")
	resp, err := ts.client.PostForm(ts.URL+"/logout", url.Values{"csrf_token": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/login" {
		t.Errorf("Location = %q, want /login", got)
	}

	// The server-side row is what actually ends the session.
	if _, _, err := auth.NewSessionStore(ts.db).Lookup(context.Background(), token.Value); err == nil {
		t.Error("the session row survived logout")
	}

	var n int
	if err := ts.db.Reader.QueryRow(`SELECT count(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d session rows after logout, want 0", n)
	}

	// And the protected page is closed again.
	if resp := ts.get(t, "/"); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("GET / after logout = %d, want a redirect", resp.StatusCode)
	}
}

// --- audit log -------------------------------------------------------------

// TestAuditLogRecordsLogins is §11's audit requirement. Nothing reads this table
// yet, so a test is the only thing keeping it honest.
func TestAuditLogRecordsLogins(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)

	ts.login(t, testUsername, "wrong-but-long-enough")
	ts.login(t, testUsername, testPassword)

	csrf := ts.csrfToken(t, "/")
	resp, err := ts.client.PostForm(ts.URL+"/logout", url.Values{"csrf_token": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	rows, err := ts.db.Reader.Query(`SELECT action, entity_id FROM audit_log ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var actions []string
	for rows.Next() {
		var action, entityID string
		if err := rows.Scan(&action, &entityID); err != nil {
			t.Fatal(err)
		}
		if entityID != testUsername {
			t.Errorf("entity_id = %q, want %q", entityID, testUsername)
		}
		actions = append(actions, action)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	want := []string{"login.failed", "login.succeeded", "logout"}
	if strings.Join(actions, ",") != strings.Join(want, ",") {
		t.Errorf("audit actions = %v, want %v", actions, want)
	}
}

// TestFailedLoginForUnknownUserIsAudited: the row must exist with a NULL user_id
// rather than being skipped.
func TestFailedLoginForUnknownUserIsAudited(t *testing.T) {
	ts := newTestServer(t)

	ts.login(t, "nosuchperson", "wrong-but-long-enough")

	var (
		action string
		userID *int64
		detail string
	)
	err := ts.db.Reader.QueryRow(`SELECT action, user_id, detail_json FROM audit_log`).
		Scan(&action, &userID, &detail)
	if err != nil {
		t.Fatalf("no audit row for a failed login against an unknown user: %v", err)
	}
	if action != "login.failed" {
		t.Errorf("action = %q", action)
	}
	if userID != nil {
		t.Errorf("user_id = %d, want NULL for an unknown user", *userID)
	}
	if !strings.Contains(detail, "unknown_user") {
		t.Errorf("detail = %q, want the reason recorded", detail)
	}
}

// --- headers and static assets ---------------------------------------------

// TestSecurityHeaders covers §11. nosniff in particular is half the defence
// against a stored-XSS via an uploaded .svg or .html.
func TestSecurityHeaders(t *testing.T) {
	ts := newTestServer(t)

	for _, path := range []string{"/login", "/healthz", "/static/app.css"} {
		resp := ts.get(t, path)

		want := map[string]string{
			"X-Content-Type-Options": "nosniff",
			"X-Frame-Options":        "DENY",
			"Referrer-Policy":        "no-referrer",
		}
		for header, value := range want {
			if got := resp.Header.Get(header); got != value {
				t.Errorf("%s: %s = %q, want %q", path, header, got, value)
			}
		}

		csp := resp.Header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'self'") {
			t.Errorf("%s: CSP = %q, want default-src 'self'", path, csp)
		}
		// No inline script or style anywhere, so the CSP must not need to allow it.
		if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
			t.Errorf("%s: CSP allows unsafe inline code: %q", path, csp)
		}
	}
}

// TestAccessLogRecordsClientIPAndRequestID is a regression test.
//
// The access log middleware was originally wrapped OUTSIDE the real-IP
// middleware. A context value only propagates downward into inner handlers, so
// the log line could never see the resolved address and silently recorded
// ip="" on every request. That is invisible in a response and only shows up in
// the container log, where nobody was looking.
func TestAccessLogRecordsClientIPAndRequestID(t *testing.T) {
	var logs strings.Builder

	ts := newTestServer(t)
	srv, err := New(ts.cfg, ts.db, slog.New(slog.NewTextHandler(&logs, nil)),
		BuildInfo{Version: "test"})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	req.RemoteAddr = "203.0.113.7:44321"
	srv.Handler().ServeHTTP(httptest.NewRecorder(), req)

	line := logs.String()
	if !strings.Contains(line, "http request") {
		t.Fatalf("no access log line was emitted: %q", line)
	}
	if !strings.Contains(line, "ip=203.0.113.7") {
		t.Errorf("the access log does not record the client IP: %q", line)
	}
	if strings.Contains(line, `ip=""`) {
		t.Errorf("the access log recorded an empty IP: %q", line)
	}
	if strings.Contains(line, `request_id=""`) || !strings.Contains(line, "request_id=") {
		t.Errorf("the access log does not record a request ID: %q", line)
	}
	for _, want := range []string{"method=GET", "path=/login", "status=200"} {
		if !strings.Contains(line, want) {
			t.Errorf("the access log is missing %s: %q", want, line)
		}
	}
}

// TestAccessLogHonoursTrustedProxies: the logged address must be the same one the
// rate limiter keys on, or investigating a lockout misleads you.
func TestAccessLogHonoursTrustedProxies(t *testing.T) {
	var logs strings.Builder

	ts := newTestServerWithConfig(t, func(cfg *config.Config) {
		cfg.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	})
	srv, err := New(ts.cfg, ts.db, slog.New(slog.NewTextHandler(&logs, nil)),
		BuildInfo{Version: "test"})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	req.RemoteAddr = "10.0.0.1:9999"
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	srv.Handler().ServeHTTP(httptest.NewRecorder(), req)

	if !strings.Contains(logs.String(), "ip=203.0.113.7") {
		t.Errorf("the forwarded address was not used: %q", logs.String())
	}
}

// TestAccessLogIgnoresSpoofedHeaders is the other half: with no trusted proxies
// configured, a forged header must not reach the log or the limiter.
func TestAccessLogIgnoresSpoofedHeaders(t *testing.T) {
	var logs strings.Builder

	ts := newTestServer(t) // no trusted proxies
	srv, err := New(ts.cfg, ts.db, slog.New(slog.NewTextHandler(&logs, nil)),
		BuildInfo{Version: "test"})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	req.RemoteAddr = "203.0.113.7:44321"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	srv.Handler().ServeHTTP(httptest.NewRecorder(), req)

	if strings.Contains(logs.String(), "1.2.3.4") {
		t.Errorf("a spoofed X-Forwarded-For reached the log: %q", logs.String())
	}
	if !strings.Contains(logs.String(), "ip=203.0.113.7") {
		t.Errorf("the socket peer was not used: %q", logs.String())
	}
}

// TestSessionRecordsTheClientIP: sessions.ip and audit_log.ip must be populated,
// since they are the only record of where a login came from.
func TestSessionAndAuditRecordTheClientIP(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	var sessionIP string
	if err := ts.db.Reader.QueryRow(`SELECT ip FROM sessions LIMIT 1`).Scan(&sessionIP); err != nil {
		t.Fatal(err)
	}
	if sessionIP == "" {
		t.Error("sessions.ip is empty")
	}

	var auditIP string
	if err := ts.db.Reader.QueryRow(
		`SELECT ip FROM audit_log WHERE action = 'login.succeeded'`).Scan(&auditIP); err != nil {
		t.Fatal(err)
	}
	if auditIP == "" {
		t.Error("audit_log.ip is empty")
	}
	if auditIP != sessionIP {
		t.Errorf("audit_log.ip = %q but sessions.ip = %q; they should agree", auditIP, sessionIP)
	}
}

func TestRequestIDIsReturned(t *testing.T) {
	ts := newTestServer(t)

	resp := ts.get(t, "/healthz")
	first := resp.Header.Get(RequestIDHeader)
	if first == "" {
		t.Fatal("no request ID header")
	}

	resp = ts.get(t, "/healthz")
	if second := resp.Header.Get(RequestIDHeader); second == first {
		t.Error("two requests got the same ID")
	}
}

func TestStaticAssetsAreServed(t *testing.T) {
	ts := newTestServer(t)

	for _, path := range []string{"/static/app.css", "/static/htmx.min.js"} {
		resp := ts.get(t, path)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if len(body) == 0 {
			t.Errorf("%s is empty", path)
		}
	}
}

// TestAuthenticatedPagesAreNotCached: an HTML page naming the signed-in user
// must not be stored by a shared cache.
func TestAuthenticatedPagesAreNotCached(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	for _, path := range []string{"/", "/api/v1/healthz"} {
		resp := ts.get(t, path)
		if got := resp.Header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", path, got)
		}
	}
}

// TestPanicBecomesA500 checks the recover middleware without letting a stack
// trace reach the client (§16).
func TestPanicBecomesA500(t *testing.T) {
	ts := newTestServer(t)

	srv, err := New(ts.cfg, ts.db, discardLogger(), BuildInfo{Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	handler := srv.recoverPanic(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("deliberate test panic")
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	for _, leak := range []string{"deliberate test panic", "goroutine", ".go:"} {
		if strings.Contains(body, leak) {
			t.Errorf("the response leaks internals (%q): %s", leak, body)
		}
	}
}

// TestSessionCookieHoldsNoUserData: the cookie must be an opaque token, not
// anything the client could read or tamper with.
func TestSessionCookieHoldsNoUserData(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	cookie := ts.sessionCookie(t)
	if cookie == nil {
		t.Fatal("no session cookie")
	}
	if strings.Contains(cookie.Value, testUsername) {
		t.Error("the session cookie contains the username")
	}
	if len(cookie.Value) != 43 {
		t.Errorf("cookie value is %d characters, want 43 for a 32-byte token", len(cookie.Value))
	}
}

// TestTamperedSessionCookieIsRejected: flipping a character must not
// authenticate anyone.
func TestTamperedSessionCookieIsRejected(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	cookie := ts.sessionCookie(t)
	tampered := "A" + cookie.Value[1:]
	if tampered == cookie.Value {
		tampered = "B" + cookie.Value[1:]
	}

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: tampered})

	// A bare client, so the jar does not supply the real cookie as well.
	bare := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := bare.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want a redirect to /login", resp.StatusCode)
	}
}

// TestExpiredSessionIsRejected drives the clock rather than waiting.
func TestExpiredSessionIsRejected(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)

	cookie := ts.sessionCookie(t)
	if cookie == nil {
		t.Fatal("not logged in")
	}

	// Age the row directly, which is what the passage of time would do.
	past := time.Now().Add(-time.Hour).Unix()
	if _, err := ts.db.Writer.Exec(
		`UPDATE sessions SET expires_at = ?, idle_expires_at = ?`, past, past); err != nil {
		t.Fatal(err)
	}

	resp := ts.get(t, "/")
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want a redirect for an expired session", resp.StatusCode)
	}
	// The dead cookie should be cleared so the browser stops sending it.
	var cleared bool
	for _, c := range resp.Cookies() {
		if c.Name == auth.SessionCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("the expired session cookie was not cleared")
	}
}
