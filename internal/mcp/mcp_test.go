package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/adeelahmad/tether/internal/cdp"
	"github.com/adeelahmad/tether/internal/cdp/cdptest"
	"github.com/adeelahmad/tether/internal/core"
	"github.com/adeelahmad/tether/internal/server"
	"github.com/adeelahmad/tether/internal/session"
)

type env struct {
	t   *testing.T
	b   *cdptest.Browser
	c   *core.Core
	srv *Server
	ts  *httptest.Server
}

func newEnv(t *testing.T, opts Options) *env {
	t.Helper()
	b := cdptest.NewBrowser()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := session.NewManager(b, session.Options{Logger: log})
	c := core.New(b, m, core.Options{Logger: log, Timeout: 5 * time.Second})
	opts.Logger = log
	srv := New(c, opts)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &env{t: t, b: b, c: c, srv: srv, ts: ts}
}

// post sends one raw body and returns the status, headers and body.
func (e *env) post(sid, body string, hdr ...string) (int, http.Header, string) {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+Prefix, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sid != "" {
		req.Header.Set(HeaderSessionID, sid)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

func (e *env) initialize() string {
	e.t.Helper()
	code, h, body := e.post("", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	if code != 200 || h.Get(HeaderSessionID) == "" {
		e.t.Fatalf("initialize: %d %s", code, body)
	}
	return h.Get(HeaderSessionID)
}

type rpcResult struct {
	Result *toolResult `json:"result"`
	Error  *rpcError   `json:"error"`
}

// call invokes a tool and returns its result.
func (e *env) call(sid, name string, args any) *toolResult {
	e.t.Helper()
	a, _ := json.Marshal(args)
	code, _, body := e.post(sid, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"`+name+`","arguments":`+string(a)+`}}`)
	if code != 200 {
		e.t.Fatalf("%s: HTTP %d %s", name, code, body)
	}
	var r rpcResult
	if err := json.Unmarshal([]byte(body), &r); err != nil || r.Error != nil || r.Result == nil {
		e.t.Fatalf("%s: %s", name, body)
	}
	return r.Result
}

func text(r *toolResult) string {
	var parts []string
	for _, c := range r.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func mustOK(t *testing.T, r *toolResult) string {
	t.Helper()
	if r.IsError {
		t.Fatalf("unexpected tool error: %s", text(r))
	}
	return text(r)
}

func mustFail(t *testing.T, r *toolResult, code string) {
	t.Helper()
	if !r.IsError || !strings.Contains(text(r), "("+code+")") {
		t.Fatalf("want error %s, got isError=%v %q", code, r.IsError, text(r))
	}
}

// page fakes in-page scripts by matching the function declaration.
func page(tab *cdptest.Tab, handlers map[string]func(cdp.CallOptions) (*cdp.Remote, error)) {
	tab.OnCall = func(decl string, opts cdp.CallOptions) (*cdp.Remote, error) {
		if h, ok := handlers[decl]; ok {
			return h(opts)
		}
		return &cdp.Remote{Type: "object", Value: json.RawMessage("null")}, nil
	}
}

func value(v any) func(cdp.CallOptions) (*cdp.Remote, error) {
	return func(cdp.CallOptions) (*cdp.Remote, error) {
		b, _ := json.Marshal(v)
		return &cdp.Remote{Value: b}, nil
	}
}

func handle(id string) func(cdp.CallOptions) (*cdp.Remote, error) {
	return func(cdp.CallOptions) (*cdp.Remote, error) { return &cdp.Remote{Type: "object", ObjectID: id}, nil }
}

// currentTab navigates so the session has a tab, and returns the fake.
func (e *env) currentTab(sid string) *cdptest.Tab {
	e.t.Helper()
	mustOK(e.t, e.call(sid, "browser_navigate", map[string]string{"url": "https://example.com/"}))
	s, err := e.c.Sessions.Get(context.Background(), e.srv.sessions[sid].currentTab())
	if err != nil {
		e.t.Fatal(err)
	}
	return s.Tab.(*cdptest.Tab)
}

func TestInitialize(t *testing.T) {
	e := newEnv(t, Options{})
	tests := []struct {
		asked, want string
	}{
		{"2025-06-18", "2025-06-18"},
		{"2025-03-26", "2025-03-26"},
		{"2024-11-05", "2024-11-05"},
		{"1999-01-01", LatestProtocolVersion},
	}
	for _, tt := range tests {
		t.Run(tt.asked, func(t *testing.T) {
			code, h, body := e.post("", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+tt.asked+`"}}`)
			var r struct {
				Result struct {
					ProtocolVersion string         `json:"protocolVersion"`
					Capabilities    map[string]any `json:"capabilities"`
					ServerInfo      struct{ Name string }
					Instructions    string
				}
			}
			_ = json.Unmarshal([]byte(body), &r)
			if code != 200 || len(h.Get(HeaderSessionID)) != 32 || r.Result.ProtocolVersion != tt.want ||
				r.Result.Capabilities["tools"] == nil || r.Result.ServerInfo.Name != "tether" || r.Result.Instructions == "" {
				t.Fatalf("%d %v %s", code, h, body)
			}
		})
	}
}

func TestToolsList(t *testing.T) {
	e := newEnv(t, Options{})
	sid := e.initialize()
	_, _, body := e.post(sid, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	var r struct {
		Result struct {
			Tools []struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				InputSchema map[string]any `json:"inputSchema"`
				Annotations map[string]any `json:"annotations"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range r.Result.Tools {
		names = append(names, tl.Name)
		if tl.Description == "" || tl.InputSchema["type"] != "object" || tl.Annotations == nil {
			t.Errorf("tool %s is under-specified", tl.Name)
		}
	}
	for _, want := range []string{"browser_tabs", "browser_new_tab", "browser_select_tab", "browser_close_tab",
		"browser_navigate", "browser_history", "browser_snapshot", "browser_click", "browser_type",
		"browser_press_key", "browser_select_option", "browser_scroll", "browser_wait_for", "browser_eval",
		"browser_screenshot", "browser_fetch"} {
		if !slices.Contains(names, want) {
			t.Errorf("missing tool %s", want)
		}
	}
}

func TestTransport(t *testing.T) {
	e := newEnv(t, Options{})
	sid := e.initialize()
	tests := []struct {
		name     string
		sid      string
		body     string
		hdr      []string
		wantCode int
		wantBody string
	}{
		{"missing session", "", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, nil, 400, "Mcp-Session-Id"},
		{"unknown session", "nope", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, nil, 404, "initialize a new one"},
		{"ping", sid, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, nil, 200, `"result":{}`},
		{"notification", sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil, 202, ""},
		{"client response", sid, `{"jsonrpc":"2.0","id":9,"result":{}}`, nil, 202, ""},
		{"unknown method", sid, `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`, nil, 200, "-32601"},
		{"unknown tool", sid, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"rm_rf"}}`, nil, 200, "-32602"},
		{"parse error", sid, `{not json`, nil, 400, "-32700"},
		{"wrong jsonrpc", sid, `{"jsonrpc":"1.0","id":1,"method":"ping"}`, nil, 400, "-32600"},
		{"bad protocol header", sid, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, []string{HeaderProtocolVersion, "1999-01-01"}, 400, "unsupported"},
		{"good protocol header", sid, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, []string{HeaderProtocolVersion, "2025-03-26"}, 200, `"result"`},
		{"wrong content type", sid, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, []string{"Content-Type", "text/plain"}, 415, "application/json"},
		{"batch", sid, `[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/x"},{"jsonrpc":"2.0","id":2,"method":"ping"}]`, nil, 200, `[{"jsonrpc":"2.0","id":1,"result":{}},{"jsonrpc":"2.0","id":2,"result":{}}]`},
		{"initialize batched", "", `[{"jsonrpc":"2.0","id":1,"method":"initialize"}]`, nil, 400, "alone"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, body := e.post(tt.sid, tt.body, tt.hdr...)
			if code != tt.wantCode || !strings.Contains(body, tt.wantBody) {
				t.Fatalf("got %d %s, want %d containing %s", code, body, tt.wantCode, tt.wantBody)
			}
		})
	}

	resp, err := http.Get(e.ts.URL + Prefix)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 405 || resp.Header.Get("Allow") != "POST, DELETE" {
		t.Fatalf("GET = %d", resp.StatusCode)
	}
	resp, _ = http.Post(e.ts.URL+"/mcp/other", "application/json", strings.NewReader("{}"))
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("subpath = %d", resp.StatusCode)
	}
}

func del(t *testing.T, url, sid string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, url+Prefix, nil)
	if sid != "" {
		req.Header.Set(HeaderSessionID, sid)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestDeleteClosesOnlyOwnTabs(t *testing.T) {
	e := newEnv(t, Options{})
	e.b.AddTab("USER", "https://example.com/inbox")
	sid := e.initialize()
	mustOK(t, e.call(sid, "browser_navigate", map[string]string{"url": "https://example.com/"}))
	mine := e.srv.sessions[sid].currentTab()
	mustOK(t, e.call(sid, "browser_select_tab", map[string]string{"tab_id": "USER"}))

	if code := del(t, e.ts.URL, sid); code != 204 {
		t.Fatalf("delete = %d", code)
	}
	open := e.b.Open()
	if slices.Contains(open, mine) || !slices.Contains(open, "USER") {
		t.Fatalf("open tabs after delete = %v (mine %s)", open, mine)
	}
	if code := del(t, e.ts.URL, sid); code != 404 {
		t.Fatalf("second delete = %d", code)
	}
	if code := del(t, e.ts.URL, ""); code != 400 {
		t.Fatalf("delete without id = %d", code)
	}
	if code, _, _ := e.post(sid, `{"jsonrpc":"2.0","id":1,"method":"ping"}`); code != 404 {
		t.Fatalf("post after delete = %d", code)
	}
}

func TestSessionExpiryAndCapacity(t *testing.T) {
	e := newEnv(t, Options{SessionTTL: time.Minute, MaxSessions: 2})
	now := time.Now()
	e.srv.now = func() time.Time { return now }
	a := e.initialize()
	tab := e.currentTab(a)
	now = now.Add(time.Second)
	b := e.initialize()
	now = now.Add(time.Second)
	_ = e.initialize() // at capacity: evicts a, the least recently used
	if code, _, _ := e.post(a, `{"jsonrpc":"2.0","id":1,"method":"ping"}`); code != 404 {
		t.Fatalf("evicted session answered %d", code)
	}
	if slices.Contains(e.b.Open(), tab.ID()) {
		t.Fatal("evicted session's tab was left open")
	}
	now = now.Add(2 * time.Minute)
	if code, _, _ := e.post(b, `{"jsonrpc":"2.0","id":1,"method":"ping"}`); code != 404 {
		t.Fatalf("expired session answered %d", code)
	}
}

func TestNavigateHonoursPolicy(t *testing.T) {
	e := newEnv(t, Options{})
	e.c.Policy = core.DomainAllowlist([]string{"example.com"})
	sid := e.initialize()

	mustFail(t, e.call(sid, "browser_navigate", map[string]string{"url": "https://evil.test/"}), core.CodeForbidden)
	out := mustOK(t, e.call(sid, "browser_navigate", map[string]string{"url": "https://www.example.com/", "wait": "domcontentloaded"}))
	if !strings.Contains(out, "Loaded https://www.example.com/ (HTTP 200)") {
		t.Fatalf("navigate = %q", out)
	}
	mustFail(t, e.call(sid, "browser_navigate", map[string]string{"url": "https://example.com/", "wait": "forever"}), codeInvalidArgs)
	mustFail(t, e.call(sid, "browser_navigate", map[string]string{"url": "https://example.com/", "bogus": "x"}), codeInvalidArgs)
	mustFail(t, e.call(sid, "browser_fetch", map[string]string{"url": "https://evil.test/api"}), core.CodeForbidden)

	// A page that drifted off the allowlist (say, via a link) cannot be read.
	tab, _ := e.c.Sessions.Get(context.Background(), e.srv.sessions[sid].currentTab())
	_, _ = tab.Tab.Navigate(context.Background(), "https://evil.test/", cdp.WaitLoad)
	mustFail(t, e.call(sid, "browser_snapshot", map[string]any{}), core.CodeForbidden)
	mustFail(t, e.call(sid, "browser_eval", map[string]string{"expression": "document.cookie"}), core.CodeForbidden)
}

func TestNoCurrentTabAndTabGone(t *testing.T) {
	e := newEnv(t, Options{})
	sid := e.initialize()
	mustFail(t, e.call(sid, "browser_snapshot", map[string]any{}), codeNoTab)
	mustFail(t, e.call(sid, "browser_close_tab", map[string]any{}), codeNoTab)

	tab := e.currentTab(sid)
	e.b.Destroy(tab.ID())
	r := e.call(sid, "browser_click", map[string]string{"selector": "a"})
	mustFail(t, r, core.CodeTabGone)
	if !strings.Contains(text(r), "browser_tabs") {
		t.Fatalf("no recovery hint: %s", text(r))
	}
	mustFail(t, e.call(sid, "browser_snapshot", map[string]any{}), codeNoTab)
	mustFail(t, e.call(sid, "browser_select_tab", map[string]string{"tab_id": "GONE"}), core.CodeTabGone)
}

func TestTabs(t *testing.T) {
	e := newEnv(t, Options{})
	e.b.AddTab("USER", "https://mail.example.com/")
	sid := e.initialize()
	out := mustOK(t, e.call(sid, "browser_tabs", map[string]any{}))
	if !strings.Contains(out, "USER") || !strings.Contains(out, "user's tab") || !strings.Contains(out, "No current tab") {
		t.Fatalf("tabs = %s", out)
	}
	mustOK(t, e.call(sid, "browser_new_tab", map[string]any{}))
	out = mustOK(t, e.call(sid, "browser_tabs", map[string]any{}))
	if !strings.Contains(out, "opened by you, current") {
		t.Fatalf("tabs = %s", out)
	}
	mustOK(t, e.call(sid, "browser_select_tab", map[string]string{"tab_id": "USER"}))
	if out := mustOK(t, e.call(sid, "browser_close_tab", map[string]any{})); !strings.Contains(out, "left open") {
		t.Fatalf("close user tab = %s", out)
	}
	if !slices.Contains(e.b.Open(), "USER") {
		t.Fatal("closed the user's own tab")
	}
	mustOK(t, e.call(sid, "browser_new_tab", map[string]string{"url": "https://example.com/"}))
	id := e.srv.sessions[sid].currentTab()
	if out := mustOK(t, e.call(sid, "browser_close_tab", map[string]string{"tab_id": id})); !strings.Contains(out, "Closed tab") {
		t.Fatalf("close = %s", out)
	}
	if slices.Contains(e.b.Open(), id) {
		t.Fatal("own tab not closed")
	}
}

func TestSnapshot(t *testing.T) {
	e := newEnv(t, Options{})
	sid := e.initialize()
	tab := e.currentTab(sid)
	snap, _ := json.Marshal(map[string]any{
		"title": "Inbox", "url": "https://example.com/", "total": 5, "text": "Hello alice", "textTotal": 11,
		"scrollY": 0, "scrollHeight": 2000, "viewport": 800,
		"items": []map[string]any{
			{"role": "button", "name": "Sign in"},
			{"role": "textbox", "name": "Password", "value": "(hidden)"},
			{"role": "link", "name": "Docs", "href": "/docs", "offscreen": true},
		},
	})
	page(tab, map[string]func(cdp.CallOptions) (*cdp.Remote, error){snapshotJS: handle("holder")})
	tab.Props = map[string][]cdp.Property{
		"holder": {{Name: "v", Value: &cdp.Remote{Value: mustJSON(string(snap))}}, {Name: "e", Value: &cdp.Remote{ObjectID: "els"}}},
		"els": {
			{Name: "0", Value: &cdp.Remote{ObjectID: "a"}}, {Name: "2", Value: &cdp.Remote{ObjectID: "c"}},
			{Name: "1", Value: &cdp.Remote{ObjectID: "b"}}, {Name: "length", Value: &cdp.Remote{Value: json.RawMessage("3")}},
		},
	}
	tab.NodeIDs = map[string]int{"a": 42, "b": 43, "c": 44}

	out := mustOK(t, e.call(sid, "browser_snapshot", map[string]int{"max_elements": 2, "max_text": 100}))
	for _, want := range []string{
		"Page: Inbox", "URL: https://example.com/", "(first 3 of 5)",
		`[e42] button "Sign in"`, `[e43] textbox "Password" value="(hidden)"`,
		`[e44] link "Docs" -> /docs (offscreen)`, "Hello alice",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("snapshot missing %q:\n%s", want, out)
		}
	}
	call := tab.Calls[len(tab.Calls)-1]
	if call.Args[0].Value != 2 || call.Args[1].Value != 100 {
		t.Errorf("limits not passed: %+v", call.Args)
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func TestClick(t *testing.T) {
	e := newEnv(t, Options{})
	sid := e.initialize()
	tab := e.currentTab(sid)
	point := map[string]any{"visible": true, "x": 10, "y": 20, "hit": true}
	page(tab, map[string]func(cdp.CallOptions) (*cdp.Remote, error){
		querySelectorJS: handle("q"),
		clickPointJS:    func(o cdp.CallOptions) (*cdp.Remote, error) { return value(point)(o) },
		jsClickJS:       value(true),
	})

	mustOK(t, e.call(sid, "browser_click", map[string]string{"ref": "e42"}))
	mustOK(t, e.call(sid, "browser_click", map[string]string{"selector": "#go"}))
	if len(tab.Clicks) != 2 || tab.Clicks[0] != [2]float64{10, 20} {
		t.Fatalf("clicks = %v", tab.Clicks)
	}

	point = map[string]any{"visible": true, "x": 1, "y": 1, "hit": false, "blocker": "div#overlay"}
	r := e.call(sid, "browser_click", map[string]string{"ref": "e42"})
	mustFail(t, r, codeNotActable)
	if !strings.Contains(text(r), "div#overlay") {
		t.Fatalf("blocker not named: %s", text(r))
	}
	mustOK(t, e.call(sid, "browser_click", map[string]any{"ref": "e42", "force": true}))
	if len(tab.Clicks) != 2 {
		t.Fatal("force click used the mouse")
	}

	mustFail(t, e.call(sid, "browser_click", map[string]any{}), codeInvalidArgs)
	mustFail(t, e.call(sid, "browser_click", map[string]string{"ref": "button"}), codeInvalidArgs)
	tab.ResolveErr = cdp.ErrNoSuchTab
	mustFail(t, e.call(sid, "browser_click", map[string]string{"ref": "e42"}), codeStaleRef)
	tab.ResolveErr = nil
	page(tab, map[string]func(cdp.CallOptions) (*cdp.Remote, error){querySelectorJS: handle("")})
	mustFail(t, e.call(sid, "browser_click", map[string]string{"selector": "#missing"}), codeNoElement)
	page(tab, map[string]func(cdp.CallOptions) (*cdp.Remote, error){
		querySelectorJS: func(cdp.CallOptions) (*cdp.Remote, error) { return nil, &cdp.JSError{Text: "SyntaxError"} },
	})
	mustFail(t, e.call(sid, "browser_click", map[string]string{"selector": "##"}), codeInvalidArgs)
}

func TestTypeAndKeys(t *testing.T) {
	e := newEnv(t, Options{})
	sid := e.initialize()
	tab := e.currentTab(sid)
	state := "ok"
	var cleared []any
	page(tab, map[string]func(cdp.CallOptions) (*cdp.Remote, error){
		focusJS: func(o cdp.CallOptions) (*cdp.Remote, error) {
			cleared = append(cleared, o.Args[1].Value)
			return value(state)(o)
		},
	})

	mustOK(t, e.call(sid, "browser_type", map[string]any{"ref": "e5", "text": "hi", "submit": true}))
	var typed strings.Builder
	for _, k := range tab.Keys {
		if k.Type == "keyDown" {
			typed.WriteString(k.Text)
		}
	}
	if typed.String() != "hi\r" {
		t.Fatalf("typed %q", typed.String())
	}
	mustOK(t, e.call(sid, "browser_type", map[string]any{"ref": "e5", "text": "x", "clear": false}))
	if len(cleared) != 2 || cleared[0] != true || cleared[1] != false {
		t.Fatalf("clear flags = %v", cleared)
	}
	for st, code := range map[string]string{"file": codeNotActable, "readonly": codeNotActable, "unfocusable": codeNotActable} {
		state = st
		mustFail(t, e.call(sid, "browser_type", map[string]any{"ref": "e5", "text": "x"}), code)
	}
	mustFail(t, e.call(sid, "browser_type", map[string]any{"ref": "e5"}), codeInvalidArgs)

	tab.Keys = nil
	mustOK(t, e.call(sid, "browser_press_key", map[string]string{"key": "Control+a"}))
	want := []string{"rawKeyDown:Control:2", "rawKeyDown:a:2", "keyUp:a:2", "keyUp:Control:0"}
	var got []string
	for _, k := range tab.Keys {
		got = append(got, k.Type+":"+k.Key+":"+itoa(k.Modifiers))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	mustFail(t, e.call(sid, "browser_press_key", map[string]string{"key": "Hyper+x"}), codeInvalidArgs)
}

func itoa(i int) string { return string(rune('0' + i)) }

func TestParseChord(t *testing.T) {
	tests := []struct {
		in   string
		mods int
		key  string
		text string
		err  bool
	}{
		{"Enter", 0, "Enter", "\r", false},
		{"enter", 0, "Enter", "\r", false},
		{"a", 0, "a", "a", false},
		{"Shift+Tab", 8, "Tab", "", false},
		{"Control+Shift+K", 10, "K", "K", false},
		{"Meta+k", 4, "k", "k", false},
		{"+", 0, "+", "+", false},
		{"Control++", 2, "+", "+", false},
		{"Shift", 0, "Shift", "", false},
		{"", 0, "", "", true},
		{"Hyper+x", 0, "", "", true},
		{"Control+Banana", 0, "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			_, mods, d, err := parseChord(tt.in)
			if (err != nil) != tt.err {
				t.Fatalf("err = %v", err)
			}
			if err == nil && (mods != tt.mods || d.key != tt.key || d.text != tt.text) {
				t.Fatalf("got mods=%d key=%q text=%q", mods, d.key, d.text)
			}
		})
	}
}

func TestSelectScrollWait(t *testing.T) {
	e := newEnv(t, Options{})
	sid := e.initialize()
	tab := e.currentTab(sid)
	sel := map[string]any{"selected": []string{"Blue"}}
	found := false
	var scrollArgs []any
	page(tab, map[string]func(cdp.CallOptions) (*cdp.Remote, error){
		selectOptionJS: func(o cdp.CallOptions) (*cdp.Remote, error) { return value(sel)(o) },
		scrollJS: func(o cdp.CallOptions) (*cdp.Remote, error) {
			scrollArgs = []any{o.Args[0].Value, o.Args[1].Value}
			return value(map[string]int{"x": 0, "y": 640, "height": 3000, "viewport": 800})(o)
		},
		scrollIntoViewJS: value(map[string]int{"y": 100}),
		waitJS:           func(o cdp.CallOptions) (*cdp.Remote, error) { return value(found)(o) },
	})

	if out := mustOK(t, e.call(sid, "browser_select_option", map[string]any{"ref": "e3", "values": []string{"blue"}})); out != "Selected Blue." {
		t.Fatalf("select = %q", out)
	}
	sel = map[string]any{"missing": []string{"pink"}, "available": []string{"Red", "Blue"}}
	r := e.call(sid, "browser_select_option", map[string]any{"ref": "e3", "values": []string{"pink"}})
	mustFail(t, r, codeNoElement)
	if !strings.Contains(text(r), "Red | Blue") {
		t.Fatalf("options not listed: %s", text(r))
	}
	mustFail(t, e.call(sid, "browser_select_option", map[string]any{"ref": "e3", "values": []string{}}), codeInvalidArgs)

	if out := mustOK(t, e.call(sid, "browser_scroll", map[string]any{})); !strings.Contains(out, "y=640") {
		t.Fatalf("scroll = %q", out)
	}
	if scrollArgs[0] != 0 || scrollArgs[1] != "page" {
		t.Fatalf("default scroll args = %v", scrollArgs)
	}
	mustOK(t, e.call(sid, "browser_scroll", map[string]any{"direction": "up", "pixels": 300}))
	if scrollArgs[1] != -300 {
		t.Fatalf("up scroll args = %v", scrollArgs)
	}
	mustOK(t, e.call(sid, "browser_scroll", map[string]any{"direction": "left"}))
	if scrollArgs[0] != "-page" || scrollArgs[1] != 0 {
		t.Fatalf("left scroll args = %v", scrollArgs)
	}
	mustOK(t, e.call(sid, "browser_scroll", map[string]any{"ref": "e9"}))
	mustFail(t, e.call(sid, "browser_scroll", map[string]any{"direction": "sideways"}), codeInvalidArgs)

	mustFail(t, e.call(sid, "browser_wait_for", map[string]any{}), codeInvalidArgs)
	mustFail(t, e.call(sid, "browser_wait_for", map[string]any{"text": "Done", "timeout_ms": 300}), core.CodeTimeout)
	found = true
	if out := mustOK(t, e.call(sid, "browser_wait_for", map[string]any{"text": "Done", "selector": "#ok"})); out != `Found "Done" #ok.` {
		t.Fatalf("wait = %q", out)
	}
}

func TestEvalScreenshotHistory(t *testing.T) {
	e := newEnv(t, Options{})
	sid := e.initialize()
	tab := e.currentTab(sid)

	tab.EvalResult = json.RawMessage(`{"n":42}`)
	if out := mustOK(t, e.call(sid, "browser_eval", map[string]string{"expression": "({n: 6*7})"})); out != `{"n":42}` {
		t.Fatalf("eval = %q", out)
	}
	tab.EvalResult = nil
	mustFail(t, e.call(sid, "browser_eval", map[string]string{"expression": " "}), codeInvalidArgs)

	page(tab, map[string]func(cdp.CallOptions) (*cdp.Remote, error){
		pageSizeJS:    value(map[string]float64{"x": 0, "y": 0, "width": 1280, "height": 50000}),
		elementRectJS: value(map[string]float64{"x": 5, "y": 6, "width": 70, "height": 80}),
	})
	r := e.call(sid, "browser_screenshot", map[string]any{})
	if r.IsError || r.Content[0].Type != "image" || r.Content[0].MimeType != "image/png" || r.Content[0].Data == "" {
		t.Fatalf("screenshot = %+v", r)
	}
	mustOK(t, e.call(sid, "browser_screenshot", map[string]any{"full_page": true}))
	mustOK(t, e.call(sid, "browser_screenshot", map[string]any{"ref": "e7"}))
	if len(tab.Shots) != 3 || tab.Shots[0] != nil || tab.Shots[1].Height != maxShotHeight ||
		*tab.Shots[2] != (cdp.Rect{X: 5, Y: 6, Width: 70, Height: 80}) {
		t.Fatalf("clips = %v %v %v", tab.Shots[0], tab.Shots[1], tab.Shots[2])
	}

	mustOK(t, e.call(sid, "browser_history", map[string]string{"action": "back"}))
	mustOK(t, e.call(sid, "browser_history", map[string]string{"action": "forward"}))
	mustOK(t, e.call(sid, "browser_history", map[string]string{"action": "reload"}))
	mustFail(t, e.call(sid, "browser_history", map[string]string{"action": "sideways"}), codeInvalidArgs)
	if !slices.Equal(tab.Moves, []int{-1, 1}) || tab.Reloads != 1 {
		t.Fatalf("history = %v reloads = %d", tab.Moves, tab.Reloads)
	}
}

func TestFetch(t *testing.T) {
	e := newEnv(t, Options{})
	sid := e.initialize()
	tab := e.currentTab(sid)
	tab.CallResult = json.RawMessage(`{"status":201,"url":"https://example.com/api","headers":[["content-type","application/json"],["set-cookie","sid=secret"]],"body":"eyJvayI6dHJ1ZX0="}`)

	out := mustOK(t, e.call(sid, "browser_fetch", map[string]any{
		"url": "https://example.com/api", "method": "post", "headers": map[string]string{"Content-Type": "application/json"}, "body": `{"q":1}`,
	}))
	for _, want := range []string{"HTTP 201", "Content-Type: application/json", `{"ok":true}`} {
		if !strings.Contains(out, want) {
			t.Errorf("fetch missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(strings.ToLower(out), "set-cookie") || strings.Contains(out, "secret") {
		t.Fatalf("fetch leaked Set-Cookie:\n%s", out)
	}
	call := tab.Calls[len(tab.Calls)-1]
	if call.Args[1].Value != "POST" {
		t.Fatalf("method = %v", call.Args[1].Value)
	}
	mustFail(t, e.call(sid, "browser_fetch", map[string]string{"url": "file:///etc/passwd"}), codeInvalidArgs)
	mustFail(t, e.call(sid, "browser_fetch", map[string]string{"url": "https://example.com/", "method": "CONNECT"}), codeInvalidArgs)
}

func TestIsTextAndTruncate(t *testing.T) {
	tests := []struct {
		ct   string
		body []byte
		want bool
	}{
		{"application/json", []byte("{}"), true},
		{"text/html; charset=utf-8", []byte("<p>"), true},
		{"image/png", []byte("\x89PNG"), false},
		{"", []byte("plain"), true},
		{"", []byte{0, 1, 2}, false},
		{"application/octet-stream", []byte("hello"), true},
	}
	for _, tt := range tests {
		if got := isText(tt.ct, tt.body); got != tt.want {
			t.Errorf("isText(%q) = %v", tt.ct, got)
		}
	}
	if got := truncate("héllo", 2); !strings.HasPrefix(got, "h\n") {
		t.Errorf("truncate split a rune: %q", got)
	}
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate = %q", got)
	}
}

func TestSnapshotFormatEmpty(t *testing.T) {
	out := (&snapshot{Title: "t", URL: "about:blank"}).format()
	if !strings.Contains(out, "(none)") || strings.Contains(out, "Text") {
		t.Fatalf("format = %q", out)
	}
}

// The MCP face sits behind the server's auth and loopback guard like every
// other face.
func TestServedBehindAuthAndGuard(t *testing.T) {
	e := newEnv(t, Options{})
	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	tests := []struct {
		name   string
		token  string
		guard  bool
		host   string
		header map[string]string
		want   int
	}{
		{"no token", "mcp-token-0123456789", false, "tether.tailnet:8787", nil, 401},
		{"token", "mcp-token-0123456789", false, "tether.tailnet:8787", map[string]string{"Authorization": "Bearer mcp-token-0123456789"}, 200},
		{"loopback", "", true, "127.0.0.1:8787", nil, 200},
		{"dns rebinding", "", true, "evil.example:8787", nil, 403},
		{"cross-origin page", "", true, "127.0.0.1:8787", map[string]string{"Origin": "https://evil.example"}, 403},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := server.New(server.Options{Token: tt.token, Guard: tt.guard, Faces: map[string]core.Face{Prefix: e.srv}}).Handler()
			r := httptest.NewRequest(http.MethodPost, Prefix, strings.NewReader(initBody))
			r.Host = tt.host
			r.Header.Set("Content-Type", "application/json")
			for k, v := range tt.header {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, tt.want, w.Body)
			}
		})
	}
}
