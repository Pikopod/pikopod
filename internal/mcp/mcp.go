package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"
)

const ProtocolVersion = "2024-11-05"

type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Annotations map[string]any
	Handler     func(ctx context.Context, args json.RawMessage) (any, error)
}

type Server struct {
	name, version string
	tools         map[string]Tool

	OnError func(error) any
}

func NewServer(name, version string) *Server {
	return &Server{name: name, version: version, tools: map[string]Tool{}}
}

func (s *Server) Register(t Tool) {
	if t.InputSchema == nil {
		t.InputSchema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	s.tools[t.Name] = t
}

func (s *Server) ToolNames() []string {
	names := make([]string, 0, len(s.tools))
	for n := range s.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	var wmu sync.Mutex
	write := func(v any) error {
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		wmu.Lock()
		defer wmu.Unlock()
		_, err = w.Write(append(raw, '\n'))
		return err
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			if err := write(response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: -32700, Message: "parse error: " + err.Error()}}); err != nil {
				return err
			}
			continue
		}
		if len(req.ID) == 0 || string(req.ID) == "null" {
			continue
		}
		res := s.handle(ctx, &req)
		if err := write(res); err != nil {
			return err
		}
	}
	return sc.Err()
}

func (s *Server) handle(ctx context.Context, req *request) response {
	res := response{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		res.Result = map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": s.name, "version": s.version},
		}
	case "ping":
		res.Result = map[string]any{}
	case "tools/list":
		list := make([]map[string]any, 0, len(s.tools))
		for _, name := range s.ToolNames() {
			t := s.tools[name]
			entry := map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema}
			if t.Annotations != nil {
				entry["annotations"] = t.Annotations
			}
			list = append(list, entry)
		}
		res.Result = map[string]any{"tools": list}
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			res.Error = &rpcError{Code: -32602, Message: "invalid params: " + err.Error()}
			return res
		}
		t, ok := s.tools[params.Name]
		if !ok {
			res.Error = &rpcError{Code: -32602, Message: fmt.Sprintf("unknown tool %q", params.Name)}
			return res
		}
		if len(params.Arguments) == 0 {
			params.Arguments = json.RawMessage("{}")
		}
		result, err := t.Handler(ctx, params.Arguments)
		isError := false
		if err != nil {
			isError = true
			if s.OnError != nil {
				result = s.OnError(err)
			} else {
				result = map[string]any{"error": err.Error()}
			}
		}
		text, _ := json.Marshal(result)
		res.Result = map[string]any{
			"content":           []map[string]any{{"type": "text", "text": string(text)}},
			"structuredContent": result,
			"isError":           isError,
		}
	default:
		res.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return res
}
