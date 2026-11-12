package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

type harness struct {
	t      *testing.T
	inW    *os.File
	outR   *os.File
	outW   *os.File
	reader *bufio.Reader
	done   chan error
}

func newHarness(t *testing.T, tools ...Tool) *harness {
	t.Helper()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, inW: inW, outR: outR, outW: outW, reader: bufio.NewReaderSize(outR, 1<<20), done: make(chan error, 1)}
	s := &Server{Name: "curia", Version: "9.9", Instructions: "be careful", Tools: tools}
	go func() {
		h.done <- s.Serve(context.Background(), inR, outW)
		_ = inR.Close()
	}()
	t.Cleanup(func() {
		_ = inW.Close()
		_ = outR.Close()
		_ = outW.Close()
	})
	return h
}

// send writes raw bytes to the server's stdin from a goroutine, so a large
// write cannot deadlock against the server's own output.
func (h *harness) send(s string) {
	h.t.Helper()
	go func() { _, _ = io.WriteString(h.inW, s+"\n") }()
}

func (h *harness) recvLine() string {
	h.t.Helper()
	type res struct {
		line string
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		l, err := h.reader.ReadString('\n')
		ch <- res{l, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			h.t.Fatalf("read: %v", r.err)
		}
		if strings.Count(r.line, "\n") != 1 {
			h.t.Fatalf("not exactly one line: %q", r.line)
		}
		return strings.TrimSuffix(r.line, "\n")
	case <-time.After(5 * time.Second):
		h.t.Fatal("timed out waiting for a response line")
		return ""
	}
}

func (h *harness) recv() map[string]any {
	h.t.Helper()
	line := h.recvLine()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		h.t.Fatalf("bad json %q: %v", line, err)
	}
	return m
}

// finish closes stdin, waits for Serve to return nil and returns any output
// lines that were not read yet.
func (h *harness) finish() []string {
	h.t.Helper()
	_ = h.inW.Close()
	select {
	case err := <-h.done:
		if err != nil {
			h.t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(time.Second):
		h.t.Fatal("Serve did not return after EOF")
	}
	_ = h.outW.Close()
	rest, _ := io.ReadAll(h.reader)
	var lines []string
	for l := range strings.SplitSeq(string(rest), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

func (h *harness) ping() {
	const id = 1
	h.t.Helper()
	h.send(`{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"ping"}`)
	m := h.recv()
	if m["id"] != float64(id) || m["result"] == nil {
		h.t.Fatalf("ping not answered: %v", m)
	}
}

func errCode(t *testing.T, m map[string]any) float64 {
	t.Helper()
	code, ok := dig(t, m, "error", "code").(float64)
	if !ok {
		t.Fatalf("no error code: %v", m)
	}
	return code
}

// dig walks maps by string key and slices by int index, failing the test on a
// wrong shape.
func dig(t *testing.T, v any, path ...any) any {
	t.Helper()
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("not an object at %q: %v", k, v)
			}
			v = m[k]
		case int:
			a, ok := v.([]any)
			if !ok || k >= len(a) {
				t.Fatalf("not an array of >%d at %d: %v", k, k, v)
			}
			v = a[k]
		}
	}
	return v
}

func okTool(name string) Tool {
	return Tool{
		Name: name, Description: name + " desc",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
		Call: func(context.Context, json.RawMessage) (any, *ToolError) {
			return map[string]any{"ok": true}, nil
		},
	}
}

func TestInitializeNegotiatesVersion(t *testing.T) {
	cases := map[string]string{
		"2025-11-25": "2025-11-25", "2025-06-18": "2025-06-18",
		"2025-03-26": "2025-03-26", "2024-11-05": "2024-11-05",
		"1999-01-01": "2025-11-25", "2026-07-28": "2025-11-25",
	}
	h := newHarness(t)
	i := 0
	for ask, want := range cases {
		i++
		h.send(`{"jsonrpc":"2.0","id":` + strconv.Itoa(i) + `,"method":"initialize","params":{"protocolVersion":"` + ask + `","capabilities":{}}}`)
		m := h.recv()
		res := dig(t, m, "result")
		if dig(t, res, "protocolVersion") != want {
			t.Errorf("ask %s: got %v, want %s", ask, dig(t, res, "protocolVersion"), want)
		}
		if dig(t, res, "instructions") != "be careful" {
			t.Errorf("instructions = %v", dig(t, res, "instructions"))
		}
		si := dig(t, res, "serverInfo")
		if dig(t, si, "name") != "curia" || dig(t, si, "version") != "9.9" {
			t.Errorf("serverInfo = %v", si)
		}
		if dig(t, res, "capabilities", "tools") == nil {
			t.Errorf("capabilities = %v", dig(t, res, "capabilities"))
		}
	}
	h.finish()
}

func TestInitializeWithoutParams(t *testing.T) {
	h := newHarness(t)
	h.send(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	res := h.recv()
	if v := dig(t, res, "result", "protocolVersion"); v != "2025-11-25" {
		t.Errorf("got %v", v)
	}
	h.finish()
}

func TestDiscoverThenInitialize(t *testing.T) {
	h := newHarness(t, okTool("a"))
	h.send(`{"jsonrpc":"2.0","id":"server-discover-1","method":"server/discover","params":{"_meta":{"x":1}}}`)
	m := h.recv()
	if errCode(t, m) != -32601 || m["id"] != "server-discover-1" {
		t.Fatalf("discover: %v", m)
	}
	if msg := dig(t, m, "error", "message"); msg != "method not found: server/discover" {
		t.Errorf("message = %v", msg)
	}
	h.send(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	if h.recv()["result"] == nil {
		t.Fatal("initialize not answered")
	}
	h.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	h.send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if h.recv()["result"] == nil {
		t.Fatal("tools/list not answered")
	}
	select {
	case err := <-h.done:
		t.Fatalf("Serve returned early: %v", err)
	default:
	}
	if rest := h.finish(); len(rest) != 0 {
		t.Errorf("extra output: %v", rest)
	}
}

func TestToolCallWithMeta(t *testing.T) {
	h := newHarness(t, okTool("a"))
	h.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"a","arguments":{"q":"x"},"_meta":{"claudecode/toolUseId":"toolu_x","progressToken":2}}}`)
	m := h.recv()
	if dig(t, m, "result", "isError") != false {
		t.Fatalf("result: %v", m)
	}
	h.finish()
}

func TestToolArgumentsPassedAndDefaulted(t *testing.T) {
	var got []string
	tool := Tool{Name: "a", Call: func(_ context.Context, args json.RawMessage) (any, *ToolError) {
		got = append(got, string(args))
		return 1, nil
	}}
	h := newHarness(t, tool)
	h.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"a","arguments":{"q":"x"}}}`)
	h.recv()
	h.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"a"}}`)
	h.recv()
	h.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"a","arguments":null}}`)
	h.recv()
	h.finish()
	want := []string{`{"q":"x"}`, `{}`, `{}`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("args = %v, want %v", got, want)
	}
}

func TestToolsListInOrder(t *testing.T) {
	h := newHarness(t, okTool("zeta"), okTool("alpha"))
	h.send(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	tools, _ := dig(t, h.recv(), "result", "tools").([]any)
	if len(tools) != 2 {
		t.Fatalf("tools = %v", tools)
	}
	for i, name := range []string{"zeta", "alpha"} {
		tl := tools[i]
		if dig(t, tl, "name") != name || dig(t, tl, "description") != name+" desc" {
			t.Errorf("tool %d = %v", i, tl)
		}
		if dig(t, tl, "inputSchema", "type") != "object" {
			t.Errorf("schema = %v", dig(t, tl, "inputSchema"))
		}
	}
	h.finish()
}

func TestToolCallResultAndError(t *testing.T) {
	bad := Tool{Name: "bad", Call: func(context.Context, json.RawMessage) (any, *ToolError) {
		return nil, &ToolError{Code: "invalid_ticket", Message: "no", Problems: []Problem{{"options", "need 2 to 4"}}}
	}}
	h := newHarness(t, okTool("good"), bad)
	h.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"good"}}`)
	res := dig(t, h.recv(), "result")
	c := dig(t, res, "content", 0)
	if dig(t, c, "type") != "text" || dig(t, c, "text") != `{"ok":true}` || dig(t, res, "isError") != false {
		t.Errorf("good = %v", res)
	}
	h.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"bad"}}`)
	res = dig(t, h.recv(), "result")
	c = dig(t, res, "content", 0)
	want := `{"error":{"code":"invalid_ticket","message":"no","problems":[{"field":"options","problem":"need 2 to 4"}]}}`
	if dig(t, res, "isError") != true || dig(t, c, "text") != want {
		t.Errorf("bad = %v", res)
	}
	h.finish()
}

func TestUnknownToolIsInvalidParams(t *testing.T) {
	h := newHarness(t, okTool("a"))
	h.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"answer_decision"}}`)
	if c := errCode(t, h.recv()); c != -32602 {
		t.Errorf("code = %v", c)
	}
	h.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{}}`)
	if c := errCode(t, h.recv()); c != -32602 {
		t.Errorf("missing name code = %v", c)
	}
	h.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":5}}`)
	if c := errCode(t, h.recv()); c != -32602 {
		t.Errorf("numeric name code = %v", c)
	}
	h.send(`{"jsonrpc":"2.0","id":4,"method":"tools/call"}`)
	if c := errCode(t, h.recv()); c != -32602 {
		t.Errorf("no params code = %v", c)
	}
	h.finish()
}

func TestProtocolErrors(t *testing.T) {
	h := newHarness(t, okTool("a"))
	cases := []struct {
		line string
		code float64
		id   any
	}{
		{`{`, -32700, nil},
		{`[]`, -32600, nil},
		{`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, -32600, nil},
		{`42`, -32600, nil},
		{`"x"`, -32600, nil},
		{`null`, -32600, nil},
		{`{"id":1,"method":"ping"}`, -32600, float64(1)},
		{`{"jsonrpc":"1.0","id":1,"method":"ping"}`, -32600, float64(1)},
		{`{"jsonrpc":2,"id":1,"method":"ping"}`, -32600, float64(1)},
		{`{"jsonrpc":"2.0","id":1,"method":7}`, -32600, float64(1)},
		{`{"jsonrpc":"2.0","id":{"a":1},"method":"ping"}`, -32600, nil},
		{`{"jsonrpc":"2.0","id":1,"method":"resources/list"}`, -32601, float64(1)},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"a","arguments":"x"}}`, -32602, float64(1)},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"a","arguments":[1]}}`, -32602, float64(1)},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"x"}`, -32602, float64(1)},
	}
	for _, c := range cases {
		h.send(c.line)
		m := h.recv()
		if errCode(t, m) != c.code || m["id"] != c.id || m["jsonrpc"] != "2.0" {
			t.Errorf("%s -> %v", c.line, m)
		}
		h.ping()
	}
	h.finish()
}

func TestNotificationsGetNoReply(t *testing.T) {
	h := newHarness(t)
	h.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	h.send(`{"jsonrpc":"2.0","method":"notifications/canceled","params":{"requestId":1}}`)
	h.send(`{"jsonrpc":"2.0","method":"something/else"}`)
	h.send(`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"nope"}}`)
	h.ping()
	if rest := h.finish(); len(rest) != 0 {
		t.Errorf("extra output: %v", rest)
	}
}

func TestOversizedLineSkipped(t *testing.T) {
	h := newHarness(t)
	h.send(`{"jsonrpc":"2.0","id":1,"method":"x","params":"` + strings.Repeat("a", 2<<20) + `"}`)
	m := h.recv()
	if errCode(t, m) != -32700 || m["id"] != nil {
		t.Fatalf("oversized: %v", m)
	}
	h.ping()
	if rest := h.finish(); len(rest) != 0 {
		t.Errorf("extra output: %v", rest)
	}
}

func TestPanicInToolIsContained(t *testing.T) {
	boom := Tool{Name: "boom", Call: func(context.Context, json.RawMessage) (any, *ToolError) { panic("kaput") }}
	h := newHarness(t, boom, okTool("a"))
	h.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"boom"}}`)
	res := dig(t, h.recv(), "result")
	text, _ := dig(t, res, "content", 0, "text").(string)
	var e struct {
		Error ToolError `json:"error"`
	}
	if err := json.Unmarshal([]byte(text), &e); err != nil {
		t.Fatal(err)
	}
	if dig(t, res, "isError") != true || e.Error.Code != "internal" {
		t.Errorf("panic result = %v", res)
	}
	h.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"a"}}`)
	if dig(t, h.recv(), "result", "isError") != false {
		t.Error("next call failed")
	}
	h.finish()
}

func TestOutputIsOneLinePerMessage(t *testing.T) {
	tool := Tool{Name: "a", Call: func(context.Context, json.RawMessage) (any, *ToolError) {
		return map[string]string{"s": "line1\nline2 <&>"}, nil
	}}
	h := newHarness(t, tool)
	h.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"a"}}`)
	line := h.recvLine()
	if strings.Contains(line, "\\u003c") || strings.Contains(line, "\\u0026") || !strings.Contains(line, "<&>") {
		t.Errorf("html escaped: %s", line)
	}
	h.finish()
}

func TestEOFEndsServe(t *testing.T) {
	h := newHarness(t)
	h.ping()
	h.finish()
}

func TestEOFWithoutTrailingNewline(t *testing.T) {
	h := newHarness(t)
	_, _ = io.WriteString(h.inW, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	_ = h.inW.Close()
	if err := <-h.done; err != nil {
		t.Fatal(err)
	}
	m := h.recv()
	if m["id"] != float64(1) {
		t.Errorf("got %v", m)
	}
}

func TestResponseMessageIgnored(t *testing.T) {
	h := newHarness(t)
	h.send(`{"jsonrpc":"2.0","id":"srv-1","result":{}}`)
	h.send(`{"jsonrpc":"2.0","id":"srv-2","error":{"code":-1,"message":"x"}}`)
	h.ping()
	if rest := h.finish(); len(rest) != 0 {
		t.Errorf("extra output: %v", rest)
	}
}

func TestContextCancelStopsServe(t *testing.T) {
	inR, inW, _ := os.Pipe()
	defer func() { _ = inW.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- (&Server{}).Serve(ctx, inR, io.Discard) }()
	cancel()
	_, _ = io.WriteString(inW, "\n")
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve ignored cancellation")
	}
}
