package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func drive(t *testing.T, s *Server, lines ...string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := s.Serve(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	var res []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad response line %q: %v", line, err)
		}
		res = append(res, m)
	}
	return res
}

func testServer() *Server {
	s := NewServer("t", "0")
	s.Register(Tool{Name: "echo", Description: "echoes", Handler: func(_ context.Context, args json.RawMessage) (any, error) {
		return map[string]any{"got": json.RawMessage(args)}, nil
	}})
	s.Register(Tool{Name: "boom", Description: "fails", Handler: func(context.Context, json.RawMessage) (any, error) {
		return nil, errors.New("no")
	}})
	return s
}

func TestInitializeListAndCall(t *testing.T) {
	res := drive(t, testServer(),
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"x":1}}}`,
	)
	if len(res) != 3 {
		t.Fatalf("notifications get no answer; want 3 responses, got %d", len(res))
	}
	init := res[0]["result"].(map[string]any)
	if init["protocolVersion"] != ProtocolVersion {
		t.Fatalf("initialize: %v", init)
	}
	tools := res[1]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 2 || tools[0].(map[string]any)["name"] != "boom" {
		t.Fatalf("tools/list must be sorted and complete: %v", tools)
	}
	call := res[2]["result"].(map[string]any)
	if call["isError"] != false || call["structuredContent"].(map[string]any)["got"].(map[string]any)["x"] != 1.0 {
		t.Fatalf("call: %v", call)
	}
}

func TestErrorsAreResultsNotProtocolFailures(t *testing.T) {
	s := testServer()
	s.OnError = func(err error) any { return map[string]any{"verdict": "ERROR", "why": err.Error()} }
	res := drive(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"boom"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"nope"}}`,
		`{"jsonrpc":"2.0","id":3,"method":"what/ever"}`,
		`not json`,
	)
	call := res[0]["result"].(map[string]any)
	if call["isError"] != true || call["structuredContent"].(map[string]any)["verdict"] != "ERROR" {
		t.Fatalf("a handler error must be a structured error result: %v", call)
	}
	if res[1]["error"] == nil || res[2]["error"] == nil || res[3]["error"] == nil {
		t.Fatalf("unknown tool, unknown method and a parse error are protocol errors: %v", res[1:])
	}
}
