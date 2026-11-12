// Package mcp is a minimal Model Context Protocol server over stdio, on the
// standard library: one JSON-RPC 2.0 message per line in each direction.
// Everything read from the input is untrusted; nothing but protocol messages
// is ever written to the output.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
)

// maxLine is the longest accepted input line, in bytes, newline excluded.
const maxLine = 1 << 20

// latestProtocol is the newest protocol version the server speaks.
const latestProtocol = "2025-11-25"

var knownProtocols = map[string]bool{
	"2025-11-25": true, "2025-06-18": true, "2025-03-26": true, "2024-11-05": true,
}

// JSON-RPC error codes.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

// Problem names one field of a tool call that was rejected, and why.
type Problem struct {
	Field   string `json:"field"`
	Problem string `json:"problem"`
}

// ToolError is a tool-level failure. The model reads it and can correct itself.
type ToolError struct {
	Code     string    `json:"code"`
	Message  string    `json:"message"`
	Problems []Problem `json:"problems,omitempty"`
}

// Tool is one callable tool. InputSchema is a JSON Schema object.
type Tool struct {
	Name, Description string
	InputSchema       json.RawMessage
	Call              func(ctx context.Context, args json.RawMessage) (result any, terr *ToolError)
}

// Server serves tools over one stdio connection.
type Server struct {
	Name, Version, Instructions string
	Tools                       []Tool
	Log                         *slog.Logger
}

// rpcError is a JSON-RPC error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// response is an outgoing JSON-RPC message. ID is already valid JSON.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// message is an incoming JSON-RPC message with every field kept raw so that
// a wrong type is a protocol error instead of a decode failure.
type message struct {
	JSONRPC json.RawMessage `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  json.RawMessage `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (s *Server) logger() *slog.Logger {
	if s.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Log
}

// Serve reads requests from in and writes responses to out, one at a time,
// until in ends (nil), ctx is canceled (ctx.Err()), or a write fails.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	r := bufio.NewReaderSize(in, 64*1024)
	for {
		line, tooLong, err := readLine(r)
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if tooLong {
			s.logger().Warn("mcp: input line too long, skipped")
			if werr := writeJSON(out, response{JSONRPC: "2.0", ID: nullID, Error: &rpcError{codeParse, "line too long"}}); werr != nil {
				return werr
			}
		} else if len(line) > 0 {
			if werr := s.handleLine(ctx, out, line); werr != nil {
				return werr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// readLine returns the next line without its terminator. A line longer than
// maxLine is consumed whole and reported with tooLong. At end of input it
// returns what was read together with the error.
func readLine(r *bufio.Reader) (line []byte, tooLong bool, err error) {
	for {
		chunk, rerr := r.ReadSlice('\n')
		if !tooLong {
			line = append(line, chunk...)
			if len(bytes.TrimRight(line, "\r\n")) > maxLine {
				tooLong, line = true, nil
			}
		}
		if errors.Is(rerr, bufio.ErrBufferFull) {
			continue
		}
		if tooLong {
			return nil, true, rerr
		}
		return bytes.TrimSpace(line), false, rerr
	}
}

var nullID = json.RawMessage("null")

func (s *Server) handleLine(ctx context.Context, out io.Writer, line []byte) error {
	reply := s.dispatch(ctx, line)
	if reply == nil {
		return nil
	}
	return writeJSON(out, reply)
}

// dispatch turns one input line into at most one response.
func (s *Server) dispatch(ctx context.Context, line []byte) *response {
	if !json.Valid(line) {
		return errResponse(nullID, codeParse, "parse error")
	}
	var msg message
	if line[0] != '{' || json.Unmarshal(line, &msg) != nil {
		return errResponse(nullID, codeInvalidRequest, "invalid request")
	}
	hasID := len(msg.ID) > 0
	if hasID && len(msg.Method) == 0 {
		return nil // a response to a request we never sent
	}
	id := nullID
	if hasID && (isString(msg.ID) || isNumber(msg.ID) || string(msg.ID) == "null") {
		id = msg.ID
	} else if hasID {
		return errResponse(nullID, codeInvalidRequest, "invalid request id")
	}
	var version, method string
	if json.Unmarshal(msg.JSONRPC, &version) != nil || version != "2.0" ||
		json.Unmarshal(msg.Method, &method) != nil {
		return errResponse(id, codeInvalidRequest, "invalid request")
	}
	if !hasID {
		return nil // notifications are never answered
	}
	return s.request(ctx, id, method, msg.Params)
}

func isString(b json.RawMessage) bool { return len(b) > 0 && b[0] == '"' }

func isNumber(b json.RawMessage) bool {
	return len(b) > 0 && (b[0] == '-' || (b[0] >= '0' && b[0] <= '9'))
}

func errResponse(id json.RawMessage, code int, msg string) *response {
	return &response{JSONRPC: "2.0", ID: id, Error: &rpcError{code, msg}}
}

func (s *Server) request(ctx context.Context, id json.RawMessage, method string, params json.RawMessage) *response {
	switch method {
	case "initialize":
		return &response{JSONRPC: "2.0", ID: id, Result: s.initResult(params)}
	case "ping":
		return &response{JSONRPC: "2.0", ID: id, Result: struct{}{}}
	case "tools/list":
		return &response{JSONRPC: "2.0", ID: id, Result: s.listResult()}
	case "tools/call":
		return s.callTool(ctx, id, params)
	default:
		return errResponse(id, codeMethodNotFound, "method not found: "+method)
	}
}

func (s *Server) initResult(params json.RawMessage) map[string]any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p) // wrong shapes fall back to the newest version
	version := latestProtocol
	if knownProtocols[p.ProtocolVersion] {
		version = p.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": struct{}{}},
		"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
		"instructions":    s.Instructions,
	}
}

func (s *Server) listResult() map[string]any {
	tools := make([]map[string]any, 0, len(s.Tools))
	for _, t := range s.Tools {
		schema := t.InputSchema
		if !json.Valid(schema) {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": schema})
	}
	return map[string]any{"tools": tools}
}

func (s *Server) callTool(ctx context.Context, id, params json.RawMessage) *response {
	var p struct {
		Name      json.RawMessage `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if len(params) == 0 || params[0] != '{' || json.Unmarshal(params, &p) != nil {
		return errResponse(id, codeInvalidParams, "params must be an object")
	}
	var name string
	if json.Unmarshal(p.Name, &name) != nil || name == "" {
		return errResponse(id, codeInvalidParams, "missing tool name")
	}
	args := p.Arguments
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	} else if args[0] != '{' {
		return errResponse(id, codeInvalidParams, "arguments must be an object")
	}
	for _, t := range s.Tools {
		if t.Name == name {
			return &response{JSONRPC: "2.0", ID: id, Result: s.runTool(ctx, t, args)}
		}
	}
	return errResponse(id, codeInvalidParams, "unknown tool: "+name)
}

// runTool calls t, containing a panic, and builds the tools/call result.
func (s *Server) runTool(ctx context.Context, t Tool, args json.RawMessage) map[string]any {
	result, terr := s.safeCall(ctx, t, args)
	if terr != nil {
		return toolResult(map[string]any{"error": terr}, true)
	}
	return toolResult(result, false)
}

func (s *Server) safeCall(ctx context.Context, t Tool, args json.RawMessage) (result any, terr *ToolError) {
	defer func() {
		if rec := recover(); rec != nil {
			s.logger().Error("mcp: tool panicked", "tool", t.Name, "panic", fmt.Sprint(rec))
			result, terr = nil, &ToolError{Code: "internal", Message: "internal error in tool " + t.Name}
		}
	}()
	return t.Call(ctx, args)
}

func toolResult(v any, isError bool) map[string]any {
	text, err := marshal(v)
	if err != nil {
		text, _ = marshal(map[string]any{"error": &ToolError{Code: "internal", Message: "result could not be encoded"}})
		isError = true
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(text)}},
		"isError": isError,
	}
}

// marshal encodes v compactly with HTML escaping off and no trailing newline.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// writeJSON writes v as one line.
func writeJSON(out io.Writer, v any) error {
	b, err := marshal(v)
	if err != nil {
		return err
	}
	_, err = out.Write(append(b, '\n'))
	return err
}
