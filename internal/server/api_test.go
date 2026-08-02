package server

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	pngenc "image/png"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/datcal/ambar/internal/auth"
	"github.com/datcal/ambar/internal/index"
)

// apiToken creates a read/write token for the test user and returns the plaintext.
func (ts *testServer) apiToken(t *testing.T) string {
	t.Helper()
	var uid int64
	if err := ts.db.Reader.QueryRow(`SELECT id FROM users WHERE username = ?`, testUsername).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	plain, _, err := auth.NewTokenStore(ts.db).Create(context.Background(), uid, "test", []string{"write"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return plain
}

func (ts *testServer) apiGet(t *testing.T, path, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// apiJSON does a GET and decodes it into `into`, failing the test on anything that is
// not a 200 with a JSON body.
func (ts *testServer) apiJSON(t *testing.T, path, token string, into any) {
	t.Helper()
	resp := ts.apiGet(t, path, token)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s = %d: %s", path, resp.StatusCode, body)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}

func TestAPIRequiresToken(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)

	// No token → 401 JSON.
	resp := ts.apiGet(t, "/api/v1/search", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token status = %d, want 401", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("error content-type = %q", ct)
	}
	// A bogus token → 401.
	if r := ts.apiGet(t, "/api/v1/search", "ambar_nope"); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("bogus token status = %d, want 401", r.StatusCode)
	}
}

func TestAPISearchAndAsset(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	id := ts.insertAsset(t, "hero.png")
	token := ts.apiToken(t)

	// Search returns the asset.
	resp := ts.apiGet(t, "/api/v1/search?q=hero", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search status = %d", resp.StatusCode)
	}
	var search struct {
		Assets []struct {
			ID       int64             `json:"id"`
			Filename string            `json:"filename"`
			Links    map[string]string `json:"links"`
		} `json:"assets"`
		Total int `json:"total"`
	}
	json.NewDecoder(resp.Body).Decode(&search)
	if search.Total != 1 || len(search.Assets) != 1 || search.Assets[0].Filename != "hero.png" {
		t.Fatalf("search result = %+v", search)
	}
	if search.Assets[0].Links["file"] == "" {
		t.Errorf("asset has no file link")
	}

	// The asset endpoint returns detail + tags.
	resp = ts.apiGet(t, itoa("/api/v1/assets/%d", id), token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("asset status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"hero.png"`) {
		t.Errorf("asset body = %s", body)
	}

	// A missing asset → 404 JSON.
	if r := ts.apiGet(t, "/api/v1/assets/99999", token); r.StatusCode != http.StatusNotFound {
		t.Errorf("missing asset status = %d, want 404", r.StatusCode)
	}
}

func TestAPITagsAutocomplete(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	id := ts.insertAsset(t, "sprite.png")
	token := ts.apiToken(t)
	// Tag it so there is something to autocomplete.
	ts.postForm(t, itoa("/assets/%d/tags", id), url.Values{"tag": {"theme:sci-fi"}})

	resp := ts.apiGet(t, "/api/v1/tags?prefix=theme", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tags status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "theme:sci-fi") {
		t.Errorf("autocomplete missing the tag: %s", body)
	}
}

// --- M18: grouped, sorted, numbered search -------------------------------------------
//
// The Godot plugin's browse was a fixed six-column grid in filename order with a "load
// more" button, which is the shape of the API it was talking to. These cover the second
// mode that replaced it — and, just as importantly, that asking for none of it still
// gets the original per-file answer.

// seedVariantLibrary writes one artwork in three formats plus a second sprite, and
// indexes them. Real files through a real scan, so the group rows are the ones the
// scanner actually produces rather than hand-built fixtures.
func (ts *testServer) seedVariantLibrary(t *testing.T) {
	t.Helper()
	ts.seedLibrary(t, map[string]string{
		"heroes/hero.png":      "png bytes",
		"heroes/hero.psd":      "psd bytes",
		"heroes/hero.aseprite": "aseprite bytes",
		"heroes/villain.png":   "png bytes",
	})
}

func TestAPISearchGroupsFormatVariants(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.seedVariantLibrary(t)
	token := ts.apiToken(t)

	var got struct {
		Assets []struct {
			Filename     string `json:"filename"`
			GroupID      int64  `json:"group_id"`
			VariantCount int    `json:"variant_count"`
		} `json:"assets"`
		Total    int    `json:"total"`
		Grouped  bool   `json:"grouped"`
		Pages    int    `json:"pages"`
		Page     int    `json:"page"`
		PageSize int    `json:"page_size"`
		Cursor   string `json:"next_cursor"`
	}
	ts.apiJSON(t, "/api/v1/search?group=1", token, &got)

	// Two logical assets, not four files (§5.1, invariant 7).
	if got.Total != 2 || len(got.Assets) != 2 {
		t.Fatalf("grouped search returned %d of %d, want 2 of 2: %+v", len(got.Assets), got.Total, got.Assets)
	}
	if !got.Grouped {
		t.Error("grouped search did not say so")
	}
	if got.Cursor != "" {
		t.Errorf("grouped search offered a cursor %q; a numbered pager must not follow one", got.Cursor)
	}
	if got.Page != 1 || got.Pages != 1 {
		t.Errorf("page %d of %d, want 1 of 1", got.Page, got.Pages)
	}

	var hero, villain int
	for _, a := range got.Assets {
		if a.GroupID == 0 {
			t.Errorf("%s has no group id", a.Filename)
		}
		switch a.Filename {
		case "hero.png":
			hero = a.VariantCount
		case "villain.png":
			villain = a.VariantCount
		default:
			t.Errorf("unexpected primary %q — a variant was surfaced as its own asset", a.Filename)
		}
	}
	if hero != 3 {
		t.Errorf("hero variant_count = %d, want 3 (png, psd, aseprite)", hero)
	}
	if villain != 1 {
		t.Errorf("villain variant_count = %d, want 1", villain)
	}
}

// TestAPISearchFlatModeUnchanged is the compatibility half: a request with none of the
// M18 parameters must behave exactly as it did, one row per file and a cursor.
func TestAPISearchFlatModeUnchanged(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.seedVariantLibrary(t)
	token := ts.apiToken(t)

	var got struct {
		Assets []struct {
			Filename     string `json:"filename"`
			VariantCount int    `json:"variant_count"`
		} `json:"assets"`
		Total   int  `json:"total"`
		Grouped bool `json:"grouped"`
	}
	ts.apiJSON(t, "/api/v1/search", token, &got)

	if got.Total != 4 || len(got.Assets) != 4 {
		t.Fatalf("flat search returned %d of %d, want 4 of 4", len(got.Assets), got.Total)
	}
	if got.Grouped {
		t.Error("flat search claimed to be grouped")
	}
	for _, a := range got.Assets {
		if a.VariantCount != 0 {
			t.Errorf("%s carries variant_count %d in flat mode, where a row is a file, not an asset",
				a.Filename, a.VariantCount)
		}
	}

	// A cursor still walks it.
	var page struct {
		Assets []struct{} `json:"assets"`
		Cursor string     `json:"next_cursor"`
	}
	ts.apiJSON(t, "/api/v1/search?limit=2", token, &page)
	if page.Cursor == "" || len(page.Assets) != 2 {
		t.Fatalf("limit=2 gave %d rows, cursor %q", len(page.Assets), page.Cursor)
	}
}

func TestAPISearchSortOrders(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{
		"pack/a.png": "a",
		"pack/b.png": "bb",
		"pack/c.png": "ccc",
	})
	token := ts.apiToken(t)

	names := func(query string) []string {
		t.Helper()
		var got struct {
			Assets []struct {
				Filename string `json:"filename"`
			} `json:"assets"`
			Sort string `json:"sort"`
		}
		ts.apiJSON(t, "/api/v1/search?"+query, token, &got)
		out := make([]string, 0, len(got.Assets))
		for _, a := range got.Assets {
			out = append(out, a.Filename)
		}
		return out
	}

	cases := []struct {
		query string
		want  []string
	}{
		{"sort=name", []string{"a.png", "b.png", "c.png"}},
		{"sort=name-desc", []string{"c.png", "b.png", "a.png"}},
		{"sort=size", []string{"c.png", "b.png", "a.png"}},
		// An order nobody defined falls back to the default rather than 400ing — a
		// stale dropdown value is not worth an error.
		{"sort=nonsense", nil},
	}
	for _, tc := range cases {
		got := names(tc.query)
		if tc.want == nil {
			if len(got) != 3 {
				t.Errorf("%s returned %d rows, want all 3 under the default order", tc.query, len(got))
			}
			continue
		}
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s = %v, want %v", tc.query, got, tc.want)
		}
	}
}

func TestAPISearchNumberedPages(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{
		"pack/a.png": "a", "pack/b.png": "b", "pack/c.png": "c",
		"pack/d.png": "d", "pack/e.png": "e",
	})
	token := ts.apiToken(t)

	type response struct {
		Assets []struct {
			Filename string `json:"filename"`
		} `json:"assets"`
		Total       int   `json:"total"`
		Page        int   `json:"page"`
		Pages       int   `json:"pages"`
		PageSize    int   `json:"page_size"`
		PageNumbers []int `json:"page_numbers"`
		FirstShown  int   `json:"first_shown"`
		LastShown   int   `json:"last_shown"`
	}

	var second response
	ts.apiJSON(t, "/api/v1/search?sort=name&limit=2&page=2", token, &second)
	if second.Page != 2 || second.Pages != 3 || second.PageSize != 2 || second.Total != 5 {
		t.Fatalf("page 2 = %+v, want page 2 of 3, size 2, total 5", second)
	}
	if len(second.Assets) != 2 || second.Assets[0].Filename != "c.png" || second.Assets[1].Filename != "d.png" {
		t.Errorf("page 2 held %+v, want c.png and d.png", second.Assets)
	}
	if second.FirstShown != 3 || second.LastShown != 4 {
		t.Errorf("page 2 shows %d–%d, want 3–4", second.FirstShown, second.LastShown)
	}
	if len(second.PageNumbers) != 3 {
		t.Errorf("page_numbers = %v, want three links for three pages", second.PageNumbers)
	}

	// The last page is short, and says so rather than reporting a range past the total.
	var third response
	ts.apiJSON(t, "/api/v1/search?sort=name&limit=2&page=3", token, &third)
	if len(third.Assets) != 1 || third.Assets[0].Filename != "e.png" {
		t.Errorf("page 3 held %+v, want just e.png", third.Assets)
	}
	if third.LastShown != 5 {
		t.Errorf("page 3 last_shown = %d, want 5", third.LastShown)
	}

	// Past the end is empty, not an error: a stale page number in a client that has
	// just changed its filters must not look like a broken server.
	var past response
	ts.apiJSON(t, "/api/v1/search?sort=name&limit=2&page=99", token, &past)
	if len(past.Assets) != 0 {
		t.Errorf("page 99 returned %d rows", len(past.Assets))
	}

	// `page` alone implies the grouped mode; forgetting `group=1` must not silently
	// answer with page 1.
	var implied response
	ts.apiJSON(t, "/api/v1/search?limit=2&page=2", token, &implied)
	if implied.Page != 2 {
		t.Errorf("page= without group= reported page %d, want 2", implied.Page)
	}
}

// TestAPIAssetDetailIsOneRequest covers what the plugin's inspector panel needs: the
// asset, its tags, its other formats and the pack's licence, in one response.
func TestAPIAssetDetailIsOneRequest(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedVariantLibrary(t)
	token := ts.apiToken(t)

	id := ts.assetID(t, "heroes/hero.png")
	ts.postForm(t, itoa("/assets/%d/tags", id), url.Values{"tag": {"theme:sci-fi"}})

	var got struct {
		Asset struct {
			Filename     string `json:"filename"`
			VariantCount int    `json:"variant_count"`
			GroupID      int64  `json:"group_id"`
		} `json:"asset"`
		Tags     []string `json:"tags"`
		Variants []struct {
			Filename string `json:"filename"`
			Ext      string `json:"ext"`
		} `json:"variants"`
		Provenance map[string]any `json:"provenance"`
	}
	ts.apiJSON(t, itoa("/api/v1/assets/%d", id), token, &got)

	if got.Asset.Filename != "hero.png" || got.Asset.VariantCount != 3 || got.Asset.GroupID == 0 {
		t.Fatalf("asset = %+v", got.Asset)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "theme:sci-fi" {
		t.Errorf("tags = %v", got.Tags)
	}
	if len(got.Variants) != 3 {
		t.Fatalf("variants = %+v, want three formats", got.Variants)
	}
	exts := map[string]bool{}
	for _, v := range got.Variants {
		exts[v.Ext] = true
	}
	for _, want := range []string{"png", "psd", "aseprite"} {
		if !exts[want] {
			t.Errorf("variants missing .%s: %+v", want, got.Variants)
		}
	}
	if got.Provenance == nil {
		t.Error("no provenance block — the panel has nothing to show for the licence")
	}
}

func TestAPISorts(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	token := ts.apiToken(t)

	var got struct {
		Sorts []struct {
			Value string `json:"value"`
			Label string `json:"label"`
		} `json:"sorts"`
		Default string `json:"default"`
	}
	ts.apiJSON(t, "/api/v1/sorts", token, &got)

	if len(got.Sorts) != len(index.SortOrders()) {
		t.Fatalf("got %d orders, want %d", len(got.Sorts), len(index.SortOrders()))
	}
	if got.Default == "" {
		t.Error("no default order named")
	}
	for _, s := range got.Sorts {
		if s.Value == "" || s.Label == "" {
			t.Errorf("order %+v is missing a value or a label", s)
		}
		// Every advertised order must actually be one the search accepts.
		if string(index.ParseSort(s.Value)) != s.Value {
			t.Errorf("advertised order %q is not one the parser keeps", s.Value)
		}
	}
}

// TestAPIPreviewNeedsATokenNotACookie is the other half of M18's preview route: the
// plugin has a bearer token and no session, and before this the full-size preview was
// reachable only with the cookie.
func TestAPIPreviewIsTokenAuthed(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{"pack/a.png": "a"})
	token := ts.apiToken(t)
	id := ts.assetID(t, "pack/a.png")

	for _, path := range []string{"/preview.webp", "/anim.gif", "/sheet.gif"} {
		url := itoa("/api/v1/assets/%d", id) + path
		if r := ts.apiGet(t, url, ""); r.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without a token = %d, want 401", path, r.StatusCode)
		}
		// The derivative itself does not exist for a fake PNG, so 404 is the right
		// answer — what matters is that the token got through the door.
		if r := ts.apiGet(t, url, token); r.StatusCode == http.StatusUnauthorized {
			t.Errorf("%s with a valid token = 401", path)
		}
	}
}

// TestAPIThumbUploadIsTokenAuthed covers M18's other half of the model-preview problem: the
// server has no renderer, the Godot plugin is *inside* one, and the endpoint that stores a
// rendered thumbnail existed only behind a session cookie.
func TestAPIThumbUploadIsTokenAuthed(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{"pack/thing.gltf": "{}"})
	token := ts.apiToken(t)
	id := ts.assetID(t, "pack/thing.gltf")

	post := func(token string, body []byte) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+itoa("/api/v1/assets/%d/thumb", id),
			bytes.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Content-Type", "image/png")
		resp, err := ts.client.Do(req)
		if err != nil {
			t.Fatalf("POST thumb: %v", err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}

	// A rendered thumbnail: not blank, or the handler rejects it — which it should.
	img := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	for y := 16; y < 48; y++ {
		for x := 16; x < 48; x++ {
			img.Set(x, y, color.NRGBA{R: 200, G: 120, B: 60, A: 255})
		}
	}
	var png bytes.Buffer
	if err := pngenc.Encode(&png, img); err != nil {
		t.Fatal(err)
	}

	// No credentials at all is refused by CSRF before token auth is reached — a POST with no
	// bearer header is, as far as that middleware can tell, a browser being led into one — so
	// 403 rather than 401. A *bad* bearer token gets the API's own answer.
	if r := post("", png.Bytes()); r.StatusCode != http.StatusForbidden {
		t.Errorf("no credentials = %d, want 403", r.StatusCode)
	}
	if r := post("ambar_not_a_real_token", png.Bytes()); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad token = %d, want 401", r.StatusCode)
	}
	if r := post(token, png.Bytes()); r.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(r.Body)
		t.Fatalf("with a token = %d (%s), want 204", r.StatusCode, body)
	}

	// It is now the asset's thumbnail, for every client — which is the point: one render
	// per model, wherever it happened.
	resp := ts.apiGet(t, itoa("/api/v1/assets/%d/thumb", id), token)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("thumb after upload = %d, want 200", resp.StatusCode)
	}

	// A blank render is refused rather than stored, so a model that failed to draw does not
	// become an empty tile for ever.
	blank := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	var blankPNG bytes.Buffer
	if err := pngenc.Encode(&blankPNG, blank); err != nil {
		t.Fatal(err)
	}
	ts.seedLibrary(t, map[string]string{"pack/other.gltf": "{ }"})
	otherID := ts.assetID(t, "pack/other.gltf")
	req, _ := http.NewRequest(http.MethodPost, ts.URL+itoa("/api/v1/assets/%d/thumb", otherID),
		bytes.NewReader(blankPNG.Bytes()))
	req.Header.Set("Authorization", "Bearer "+token)
	resp2, err := ts.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("blank render = %d, want 400", resp2.StatusCode)
	}
}
