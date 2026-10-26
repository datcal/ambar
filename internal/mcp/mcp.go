// Package mcp serves tools to AI assistants over the Model Context Protocol.
//
// Only the slice of MCP this server uses: tools, over Streamable HTTP, stateless. Every POST
// carries one JSON-RPC message (or a batch) and is answered in the response body as
// application/json. There are no sessions and no server-sent events, because every tool here
// answers within its own request, so there is nothing to stream. Each request also stands on
// its own, with nothing held between them in a single NAS process.
//
// Authentication is not this package's business. The server mounts the handler behind the
// API token check, exactly like /api/v1, so an assistant is configured with the same kind of
// token as an editor plugin.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
)

// Versions are the protocol revisions this server speaks, newest first. A client asking for
// one of them gets it; any other gets the newest, and decides for itself whether to go on.
var Versions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}

// Tool is one thing an assistant can call.
type Tool struct {
	Name        string
	Title       string
	Description string
	// InputSchema is the JSON Schema of the arguments object.
	InputSchema map[string]any
	// ReadOnly marks a tool that changes nothing, which lets a client run it without asking.
	ReadOnly bool
	// Call runs the tool. An error is reported to the model as a failed call (isError), in
	// its own words, which is what lets it correct itself; protocol errors are this package's.
	Call func(ctx context.Context, args json.RawMessage) (*Result, error)
}

// Content is one block of a tool's answer.
type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

// Result is a tool's answer.
type Result struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

// Text is a plain-text answer.
func Text(format string, args ...any) *Result {
	return &Result{Content: []Content{{Type: "text", Text: fmt.Sprintf(format, args...)}}}
}

// Server answers MCP requests for a fixed set of tools.
type Server struct {
	Name    string
	Version string
	// Instructions tell the model what the server is for and how its tools fit together.
	Instructions string
	Tools        []Tool
}

// JSON-RPC error codes.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeNoMethod       = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
)

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// maxBody bounds a request. Tool arguments here are a query and a handful of ids.
const maxBody = 1 << 20

// ServeHTTP handles POST; the stream a GET would open is not offered (405, as the transport
// allows).
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "this MCP server answers POST only; it offers no event stream", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		writeJSON(w, http.StatusRequestEntityTooLarge, response{JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &rpcError{Code: codeInvalidRequest, Message: "request too large"}})
		return
	}

	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(trimmed, &batch); err != nil {
			writeJSON(w, http.StatusOK, parseError())
			return
		}
		var out []response
		for _, raw := range batch {
			if resp, ok := s.handle(r.Context(), raw); ok {
				out = append(out, resp)
			}
		}
		if len(out) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	resp, ok := s.handle(r.Context(), trimmed)
	if !ok {
		// A notification or a response: nothing to answer.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func parseError() response {
	return response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: codeParse, Message: "parse error"}}
}

// handle answers one message; ok is false when it needs no answer.
func (s *Server) handle(ctx context.Context, raw json.RawMessage) (response, bool) {
	var msg message
	if err := json.Unmarshal(raw, &msg); err != nil {
		return parseError(), true
	}
	if len(msg.ID) == 0 || string(msg.ID) == "null" {
		// Notifications (initialized, cancelled) and responses to requests this server never
		// makes.
		return response{}, false
	}
	fail := func(code int, text string) (response, bool) {
		return response{JSONRPC: "2.0", ID: msg.ID, Error: &rpcError{Code: code, Message: text}}, true
	}
	if msg.JSONRPC != "2.0" || msg.Method == "" {
		return fail(codeInvalidRequest, "not a JSON-RPC 2.0 request")
	}

	var result any
	switch msg.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		version := Versions[0]
		if slices.Contains(Versions, p.ProtocolVersion) {
			version = p.ProtocolVersion
		}
		init := map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
		}
		if s.Instructions != "" {
			init["instructions"] = s.Instructions
		}
		result = init
	case "ping":
		result = map[string]any{}
	case "tools/list":
		tools := make([]map[string]any, 0, len(s.Tools))
		for _, t := range s.Tools {
			entry := map[string]any{
				"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema,
				"annotations": map[string]any{"readOnlyHint": t.ReadOnly, "openWorldHint": false},
			}
			if t.Title != "" {
				entry["title"] = t.Title
			}
			tools = append(tools, entry)
		}
		result = map[string]any{"tools": tools}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return fail(codeInvalidParams, "tools/call takes a name and arguments")
		}
		i := slices.IndexFunc(s.Tools, func(t Tool) bool { return t.Name == p.Name })
		if i < 0 {
			return fail(codeInvalidParams, "unknown tool: "+p.Name)
		}
		args := p.Arguments
		if len(args) == 0 || string(args) == "null" {
			args = json.RawMessage("{}")
		}
		res, err := s.Tools[i].Call(ctx, args)
		switch {
		case err != nil:
			res = Text("%s", err.Error())
			res.IsError = true
		case res == nil:
			return fail(codeInternal, "the tool returned nothing")
		}
		result = res
	default:
		return fail(codeNoMethod, "method not supported: "+msg.Method)
	}
	return response{JSONRPC: "2.0", ID: msg.ID, Result: result}, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
