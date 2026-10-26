package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testServer() *Server {
	return &Server{
		Name: "ambar", Version: "test", Instructions: "find assets",
		Tools: []Tool{
			{
				Name: "echo", Description: "says it back", ReadOnly: true,
				InputSchema: map[string]any{"type": "object"},
				Call: func(_ context.Context, args json.RawMessage) (*Result, error) {
					var a struct{ Say string }
					if err := json.Unmarshal(args, &a); err != nil {
						return nil, err
					}
					if a.Say == "" {
						return nil, errors.New("say what?")
					}
					return Text("%s", a.Say), nil
				},
			},
		},
	}
}

func post(t *testing.T, body string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	testServer().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body)))
	b, _ := io.ReadAll(rec.Body)
	return rec.Code, string(b)
}

func TestInitializeAgreesOnAVersion(t *testing.T) {
	_, body := post(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`)
	for _, want := range []string{`"protocolVersion":"2025-06-18"`, `"tools":{`, `"name":"ambar"`, `"instructions":"find assets"`} {
		if !strings.Contains(body, want) {
			t.Errorf("initialize answer lacks %s:\n%s", want, body)
		}
	}
	_, body = post(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	if !strings.Contains(body, `"protocolVersion":"`+Versions[0]+`"`) {
		t.Errorf("an unknown version was not answered with the newest:\n%s", body)
	}
}

func TestNotificationsGetNoAnswer(t *testing.T) {
	if code, body := post(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != http.StatusAccepted || body != "" {
		t.Errorf("a notification = %d %q, want 202 and nothing", code, body)
	}
}

func TestToolsAreListedAndCalled(t *testing.T) {
	_, body := post(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if !strings.Contains(body, `"name":"echo"`) || !strings.Contains(body, `"readOnlyHint":true`) {
		t.Errorf("tools/list:\n%s", body)
	}

	_, body = post(t, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"say":"hello"}}}`)
	if !strings.Contains(body, `"text":"hello"`) || strings.Contains(body, "isError") {
		t.Errorf("a successful call:\n%s", body)
	}

	// A tool's own failure reaches the model as a result it can read, not a protocol error.
	_, body = post(t, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"echo","arguments":{}}}`)
	if !strings.Contains(body, `"isError":true`) || !strings.Contains(body, "say what?") {
		t.Errorf("a failing call:\n%s", body)
	}

	_, body = post(t, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"nope"}}`)
	if !strings.Contains(body, `"code":-32602`) {
		t.Errorf("an unknown tool:\n%s", body)
	}
	_, body = post(t, `{"jsonrpc":"2.0","id":6,"method":"resources/list"}`)
	if !strings.Contains(body, `"code":-32601`) {
		t.Errorf("an unsupported method:\n%s", body)
	}
}

func TestBatchesAndBadBodies(t *testing.T) {
	_, body := post(t, `[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"}]`)
	var out []map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil || len(out) != 1 {
		t.Errorf("a batch of a request and a notification = %s, want one answer", body)
	}
	if _, body := post(t, `{not json`); !strings.Contains(body, `"code":-32700`) {
		t.Errorf("a body that is not JSON:\n%s", body)
	}

	rec := httptest.NewRecorder()
	testServer().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d, want 405: there is no event stream", rec.Code)
	}
}
