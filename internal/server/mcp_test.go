package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/datcal/ambar/internal/handoff"
)

// mcpCall sends one JSON-RPC request to /mcp and returns the HTTP status and the decoded
// answer.
func (ts *testServer) mcpCall(t *testing.T, token, method string, params any) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// mcpTool calls a tool and returns its text and whether it reported an error.
func (ts *testServer) mcpTool(t *testing.T, token, name string, args map[string]any) (string, bool) {
	t.Helper()
	status, out := ts.mcpCall(t, token, "tools/call", map[string]any{"name": name, "arguments": args})
	if status != http.StatusOK {
		t.Fatalf("%s: status %d", name, status)
	}
	result, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("%s: no result in %v", name, out)
	}
	var text strings.Builder
	for _, c := range result["content"].([]any) {
		if s, ok := c.(map[string]any)["text"].(string); ok {
			text.WriteString(s)
		}
	}
	isError, _ := result["isError"].(bool)
	return text.String(), isError
}

func (ts *testServer) assetIDByPath(t *testing.T, rel string) int64 {
	t.Helper()
	var id int64
	if err := ts.db.Reader.QueryRow(`
		SELECT a.id FROM assets a JOIN packs p ON p.id = a.pack_id
		WHERE p.library_rel_path || '/' || a.rel_path = ?`, rel).Scan(&id); err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	return id
}

// seedBoat is a model that ships as OBJ and FBX, and a sprite elsewhere.
func (ts *testServer) seedBoat(t *testing.T) (obj, fbx int64) {
	t.Helper()
	ts.seedImages(t, map[string][]byte{
		"props/pirate/boat.obj": []byte("o boat\nv 0 0 0\nv 1 0 0\nv 0 1 0\nf 1 2 3\n"),
		"props/pirate/boat.fbx": []byte("Kaydara FBX Binary  \x00 not really"),
		"ui/button.png":         spritePNG(t, 32, 32),
	})
	return ts.assetIDByPath(t, "props/pirate/boat.obj"), ts.assetIDByPath(t, "props/pirate/boat.fbx")
}

// TestMCPFindsAndDescribesAssets: an assistant connects with an API token, learns the
// tools, and finds an asset by name, by folder, and by browsing, and is told which of its
// formats Unreal should take.
func TestMCPFindsAndDescribesAssets(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	token := ts.readToken(t)
	obj, fbx := ts.seedBoat(t)

	if status, _ := ts.mcpCall(t, "", "initialize", map[string]any{}); status == http.StatusOK {
		t.Error("/mcp answered without a token")
	}
	status, out := ts.mcpCall(t, token, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	if status != http.StatusOK || !strings.Contains(fmt.Sprint(out["result"]), "ambar") {
		t.Fatalf("initialize = %d %v", status, out)
	}
	_, out = ts.mcpCall(t, token, "tools/list", nil)
	listed := fmt.Sprint(out["result"])
	for _, name := range []string{"search_assets", "browse_folder", "get_asset", "view_asset", "import_into_unreal", "import_status"} {
		if !strings.Contains(listed, "name:"+name) {
			t.Errorf("tools/list lacks %s", name)
		}
	}

	text, isErr := ts.mcpTool(t, token, "search_assets", map[string]any{"query": "boat"})
	if isErr || !strings.Contains(text, "1 assets") || !strings.Contains(text, "props/pirate/boat.") ||
		!strings.Contains(text, "FBX") || !strings.Contains(text, "OBJ") {
		t.Errorf("search for boat:\n%s", text)
	}
	if text, _ := ts.mcpTool(t, token, "search_assets", map[string]any{"folder": "pirate"}); !strings.Contains(text, "in props/pirate") {
		t.Errorf("a folder by name did not scope the search:\n%s", text)
	}
	if text, isErr := ts.mcpTool(t, token, "search_assets", map[string]any{"folder": "nowhere-at-all"}); !isErr {
		t.Errorf("an unknown folder was not an error:\n%s", text)
	}

	if text, _ := ts.mcpTool(t, token, "browse_folder", nil); !strings.Contains(text, "- props/") || !strings.Contains(text, "- ui/") {
		t.Errorf("the top of the library:\n%s", text)
	}
	if text, _ := ts.mcpTool(t, token, "browse_folder", map[string]any{"path": "props"}); !strings.Contains(text, "- pirate/ (1)") {
		t.Errorf("props:\n%s", text)
	}

	text, _ = ts.mcpTool(t, token, "get_asset", map[string]any{"id": obj})
	if want := fmt.Sprintf("For Unreal: #%d (.fbx)", fbx); !strings.Contains(text, want) {
		t.Errorf("get_asset does not say %q:\n%s", want, text)
	}
	if text, isErr := ts.mcpTool(t, token, "get_asset", map[string]any{"id": 999999}); !isErr || !strings.Contains(text, "no asset #999999") {
		t.Errorf("an unknown id: %v\n%s", isErr, text)
	}
}

// TestMCPImportGoesThroughTheEditor: import_into_unreal is refused for a read token and
// when no editor is connected; with the user's editor polling, the editor gets the FBX of
// an asset asked for by its OBJ, and the assistant gets the editor's report.
func TestMCPImportGoesThroughTheEditor(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.srv.importWait = 10 * time.Second
	obj, fbx := ts.seedBoat(t)
	write, read := ts.apiToken(t), ts.readToken(t)

	if text, isErr := ts.mcpTool(t, read, "import_into_unreal", map[string]any{"asset_ids": []int64{obj}}); !isErr || !strings.Contains(text, "write scope") {
		t.Errorf("a read token imported: %v\n%s", isErr, text)
	}
	if text, isErr := ts.mcpTool(t, write, "import_into_unreal", map[string]any{"asset_ids": []int64{obj}}); !isErr || !strings.Contains(text, "no Unreal editor is connected") {
		t.Errorf("an import with no editor: %v\n%s", isErr, text)
	}

	// The editor: one long poll, then its report.
	took := make(chan handoff.Request, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/editor/requests?engine=unreal&project=OceanRaiders&wait=10", nil)
		req.Header.Set("Authorization", "Bearer "+write)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			close(took)
			return
		}
		var got struct{ Requests []handoff.Request }
		_ = json.NewDecoder(resp.Body).Decode(&got)
		resp.Body.Close()
		if len(got.Requests) != 1 {
			close(took)
			return
		}
		r := got.Requests[0]
		took <- r
		report, _ := json.Marshal(handoff.Result{Items: []handoff.Item{{
			AssetID: r.AssetIDs[0], OK: true, Path: "/Game/Ambar/props/pirate/boat", Message: "Imported",
		}}})
		post, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/v1/editor/requests/%d/result", ts.URL, r.ID), bytes.NewReader(report))
		post.Header.Set("Authorization", "Bearer "+write)
		post.Header.Set("Content-Type", "application/json")
		if resp, err := http.DefaultClient.Do(post); err == nil {
			resp.Body.Close()
		}
	}()
	var uid int64
	if err := ts.db.Reader.QueryRow(`SELECT id FROM users WHERE username = ?`, testUsername).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(ts.srv.handoff.Editors(uid, "unreal")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the editor's poll never arrived")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if text, isErr := ts.mcpTool(t, write, "import_into_unreal", map[string]any{"asset_ids": []int64{obj}, "project": "Elsewhere"}); !isErr || !strings.Contains(text, "OceanRaiders") {
		t.Errorf("an import for a project that is not open: %v\n%s", isErr, text)
	}

	text, isErr := ts.mcpTool(t, write, "import_into_unreal", map[string]any{"asset_ids": []int64{obj}})
	r, ok := <-took
	if !ok {
		t.Fatal("the editor got no request")
	}
	if len(r.AssetIDs) != 1 || r.AssetIDs[0] != fbx {
		t.Errorf("the editor was asked for %v, want the FBX #%d", r.AssetIDs, fbx)
	}
	if isErr || !strings.Contains(text, "/Game/Ambar/props/pirate/boat") || !strings.Contains(text, "done in OceanRaiders") ||
		!strings.Contains(text, "the format Unreal takes") {
		t.Errorf("the assistant's answer: %v\n%s", isErr, text)
	}

	status, _ := ts.mcpTool(t, write, "import_status", map[string]any{"request_id": r.ID})
	if !strings.Contains(status, "/Game/Ambar/props/pirate/boat") {
		t.Errorf("import_status:\n%s", status)
	}
}
