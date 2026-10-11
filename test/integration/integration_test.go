//go:build integration

// Package integration runs Tether end to end against a real headless
// Chrome: the proxy, the control API and the WebDriver face, all through
// the same tab sessions.
//
//	make test-integration            # or
//	go test -tags integration ./test/integration/
//
// Chrome is found via $TETHER_CHROME_BIN or the usual install locations.
package integration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/adeelahmad/tether/internal/app"
	"github.com/adeelahmad/tether/internal/cdp"
	"github.com/adeelahmad/tether/internal/config"
	"github.com/adeelahmad/tether/internal/mcp"
	"github.com/adeelahmad/tether/internal/proxy"
	"github.com/adeelahmad/tether/internal/webdriver"
)

// site is a tiny "logged-in" web app: /login sets a session cookie,
// /me renders the user with server HTML plus JavaScript.
func site() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: "alice", Path: "/"})
		http.Redirect(w, r, "/me", http.StatusFound)
	})
	mux.HandleFunc("/me", func(w http.ResponseWriter, r *http.Request) {
		user := "anonymous"
		if c, err := r.Cookie("sid"); err == nil {
			user = c.Value
		}
		if _, err := r.Cookie(proxy.CookieName); err == nil {
			user = "LEAKED-TETHER-COOKIE"
		}
		fmt.Fprintf(w, `<!doctype html><html><head><title>Hello %s</title></head><body>
<h1 id="user">%s</h1><p id="js"></p>
<script>document.getElementById('js').textContent = 'rendered-by-js';</script>
<form action="/search" method="get"><input id="q" name="q"><button id="go">Go</button></form>
<a id="link" href="/me?clicked=1">again</a>
</body></html>`, user, user)
	})
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<!doctype html><title>results</title><h1 id="r">%s</h1>`, r.URL.Query().Get("q"))
	})
	mux.HandleFunc("/api/echo", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c, _ := r.Cookie("sid")
		sid := ""
		if c != nil {
			sid = c.Value
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"method": r.Method, "body": string(body), "sid": sid})
	})
	return httptest.NewServer(mux)
}

func startTether(t *testing.T) (base string, client *http.Client) {
	t.Helper()
	if _, err := cdp.FindChrome(); err != nil {
		if os.Getenv("TETHER_REQUIRE_CHROME") != "" {
			t.Fatal("chrome required but not found:", err)
		}
		t.Skip("chrome not available:", err)
	}
	dir := t.TempDir()
	cfg := &config.Config{
		Addr: "127.0.0.1:0", Chrome: "auto", Launch: true, Headless: true,
		ProfileDir: dir + "/profile", DataDir: dir, IdleTimeout: time.Minute, Timeout: 30 * time.Second,
		LogLevel: "debug", ChromeArgs: strings.Fields(os.Getenv("TETHER_CHROME_ARGS")), MCP: true,
	}
	ctx, cancel := context.WithCancel(context.Background())
	log := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	a, err := app.Start(ctx, cfg, log)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- a.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	base = "http://" + a.Addr.String()
	pu, _ := url.Parse(base)
	client = &http.Client{
		Transport:     &http.Transport{Proxy: http.ProxyURL(pu)},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       60 * time.Second,
	}
	return base, client
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

func send(t *testing.T, c *http.Client, method, u string, body io.Reader, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestProxyAndAPI(t *testing.T) {
	s := site()
	defer s.Close()
	base, c := startTether(t)
	direct := &http.Client{Timeout: 30 * time.Second}

	// First contact: a fresh tab, announced by header and cookie.
	resp, body := send(t, c, "GET", s.URL+"/me", nil, nil)
	tab := resp.Header.Get(proxy.HeaderTabID)
	if resp.StatusCode != 200 || tab == "" || len(resp.Cookies()) != 1 {
		t.Fatalf("first contact: %d %v", resp.StatusCode, resp.Header)
	}
	if !strings.Contains(body, "anonymous") || !strings.Contains(body, "rendered-by-js") {
		t.Fatalf("expected rendered DOM, got %s", body)
	}

	// Log in through the proxy: the browser keeps the cookie, and the
	// client never sees it.
	resp, body = send(t, c, "GET", s.URL+"/login", nil, map[string]string{"Cookie": proxy.CookieName + "=" + tab})
	if !strings.Contains(body, `id="user">alice<`) {
		t.Fatalf("login not reflected: %s", body)
	}
	if resp.Header.Get(proxy.HeaderFinalURL) != s.URL+"/me" {
		t.Fatalf("final url = %s", resp.Header.Get(proxy.HeaderFinalURL))
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == "sid" {
			t.Fatal("site cookie leaked to the client")
		}
	}

	// A different client, no cookie: the default domain tab is already
	// logged in, and Tether's cookie is never forwarded to the site.
	_, body = send(t, c, "GET", s.URL+"/me", nil, map[string]string{"Cookie": proxy.CookieName + "=" + tab})
	if strings.Contains(body, "LEAKED") || !strings.Contains(body, "alice") {
		t.Fatalf("got %s", body)
	}

	// POST runs as fetch() in the tab, with the browser's cookies.
	resp, body = send(t, c, "POST", s.URL+"/api/echo", strings.NewReader("hello"), map[string]string{"Content-Type": "text/plain"})
	if resp.StatusCode != 201 || !strings.Contains(body, `"sid":"alice"`) || !strings.Contains(body, `"body":"hello"`) {
		t.Fatalf("fetch: %d %s", resp.StatusCode, body)
	}

	// Raw mode returns original bytes, not the live DOM.
	_, body = send(t, c, "GET", s.URL+"/me", nil, map[string]string{proxy.HeaderMode: "raw"})
	if strings.Contains(body, "rendered-by-js</p>") {
		t.Fatalf("raw mode returned rendered DOM")
	}

	// JavaScript evaluation in the tab.
	resp, body = send(t, direct, "POST", base+"/tether/v1/tabs/"+tab+"/eval",
		strings.NewReader(`{"expression":"document.getElementById('user').textContent + '@' + document.cookie"}`), nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "alice@sid=alice") {
		t.Fatalf("eval: %d %s", resp.StatusCode, body)
	}

	// Explicit close, then a well-defined error for the dead tab.
	if resp, _ := send(t, direct, "DELETE", base+"/tether/v1/tabs/"+tab, nil, nil); resp.StatusCode != 204 {
		t.Fatalf("close: %d", resp.StatusCode)
	}
	resp, body = send(t, c, "GET", s.URL+"/me", nil, map[string]string{proxy.HeaderTabID: tab})
	if resp.StatusCode != http.StatusGone || !strings.Contains(body, "tab_gone") {
		t.Fatalf("dead tab: %d %s", resp.StatusCode, body)
	}

	// Navigation failures are a 502, not a hang.
	resp, _ = send(t, c, "GET", "http://127.0.0.1:1/", nil, nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("unreachable host: %d", resp.StatusCode)
	}
}

type wd struct {
	t    *testing.T
	base string
	sid  string
}

func (w *wd) call(method, path string, body any) any {
	w.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, w.base+"/session/"+w.sid+path, r)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Value any `json:"value"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 200 {
		w.t.Fatalf("%s %s: %d %v", method, path, resp.StatusCode, out.Value)
	}
	return out.Value
}

func (w *wd) find(css string) string {
	v := w.call("POST", "/element", map[string]string{"using": "css selector", "value": css})
	return v.(map[string]any)[webdriver.ElementKey].(string)
}

func TestWebDriver(t *testing.T) {
	s := site()
	defer s.Close()
	base, _ := startTether(t)

	resp, err := http.Post(base+"/session", "application/json",
		strings.NewReader(`{"capabilities":{"alwaysMatch":{"browserName":"chrome"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	var ns struct {
		Value struct {
			SessionID string `json:"sessionId"`
		} `json:"value"`
	}
	json.NewDecoder(resp.Body).Decode(&ns)
	resp.Body.Close()
	w := &wd{t: t, base: base, sid: ns.Value.SessionID}
	if w.sid == "" {
		t.Fatal("no session id")
	}

	w.call("POST", "/url", map[string]string{"url": s.URL + "/login"})
	if got := w.call("GET", "/title", nil); got != "Hello alice" {
		t.Fatalf("title = %v", got)
	}
	if got := w.call("GET", "/url", nil); got != s.URL+"/me" {
		t.Fatalf("url = %v", got)
	}
	user := w.find("#user")
	if got := w.call("GET", "/element/"+user+"/text", nil); got != "alice" {
		t.Fatalf("text = %v", got)
	}
	xp := w.call("POST", "/element", map[string]string{"using": "xpath", "value": "//h1"}).(map[string]any)[webdriver.ElementKey]
	if xp != user {
		t.Fatalf("same node gave different references: %v vs %v", xp, user)
	}
	all := w.call("POST", "/elements", map[string]string{"using": "tag name", "value": "p"}).([]any)
	if len(all) != 1 {
		t.Fatalf("elements = %v", all)
	}

	got := w.call("POST", "/execute/sync", map[string]any{
		"script": "return [arguments[0].id, document.querySelector('#js').textContent, arguments[1] * 2]",
		"args":   []any{map[string]string{webdriver.ElementKey: user}, 21},
	})
	if b, _ := json.Marshal(got); string(b) != `["user","rendered-by-js",42]` {
		t.Fatalf("execute = %s", b)
	}
	if got := w.call("POST", "/execute/async", map[string]any{"script": "const done = arguments[0]; setTimeout(() => done('later'), 20)", "args": []any{}}); got != "later" {
		t.Fatalf("async = %v", got)
	}

	q := w.find("#q")
	w.call("POST", "/element/"+q+"/value", map[string]string{"text": "tether"})
	if got := w.call("GET", "/element/"+q+"/property/value", nil); got != "tether" {
		t.Fatalf("typed value = %v", got)
	}
	w.call("POST", "/element/"+q+"/value", map[string]string{"text": ""}) // Enter submits
	if got := w.call("GET", "/url", nil); got != s.URL+"/search?q=tether" {
		t.Fatalf("after enter url = %v", got)
	}
	w.call("POST", "/back", map[string]any{})
	w.call("POST", "/element/"+w.find("#link")+"/click", map[string]any{})
	if got := w.call("GET", "/url", nil); got != s.URL+"/me?clicked=1" {
		t.Fatalf("after click url = %v", got)
	}

	cookies := w.call("GET", "/cookie", nil).([]any)
	if len(cookies) != 1 || cookies[0].(map[string]any)["name"] != "sid" {
		t.Fatalf("cookies = %v", cookies)
	}
	w.call("POST", "/cookie", map[string]any{"cookie": map[string]any{"name": "theme", "value": "dark"}})
	if got := w.call("GET", "/cookie/theme", nil).(map[string]any)["value"]; got != "dark" {
		t.Fatalf("cookie = %v", got)
	}

	png := w.call("GET", "/screenshot", nil).(string)
	if raw, err := base64.StdEncoding.DecodeString(png); err != nil || !bytes.HasPrefix(raw, []byte("\x89PNG")) {
		t.Fatal("screenshot is not a PNG")
	}
	if src := w.call("GET", "/source", nil).(string); !strings.Contains(src, "rendered-by-js") {
		t.Fatal("page source missing rendered content")
	}

	// Stale elements after navigation.
	w.call("POST", "/url", map[string]string{"url": s.URL + "/search?q=x"})
	req, _ := http.NewRequest("GET", base+"/session/"+w.sid+"/element/"+user+"/text", nil)
	resp, _ = http.DefaultClient.Do(req)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 404 || !strings.Contains(string(b), "stale element reference") {
		t.Fatalf("stale: %d %s", resp.StatusCode, b)
	}

	req, _ = http.NewRequest("DELETE", base+"/session/"+w.sid, nil)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("delete session: %v", err)
	}
}

// mcpClient is a minimal Streamable HTTP MCP client.
type mcpClient struct {
	t    *testing.T
	url  string
	sid  string
	next int
}

func (m *mcpClient) rpc(method string, params any) json.RawMessage {
	m.t.Helper()
	m.next++
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.next, "method": method, "params": params})
	req, _ := http.NewRequest("POST", m.url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if m.sid != "" {
		req.Header.Set(mcp.HeaderSessionID, m.sid)
		req.Header.Set(mcp.HeaderProtocolVersion, mcp.LatestProtocolVersion)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		m.t.Fatal(err)
	}
	defer resp.Body.Close()
	if id := resp.Header.Get(mcp.HeaderSessionID); id != "" {
		m.sid = id
	}
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Error != nil {
		m.t.Fatalf("%s: %d %v %+v", method, resp.StatusCode, err, out.Error)
	}
	return out.Result
}

type mcpContent struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

// tool calls a tool and returns its first content block, failing on a
// tool error.
func (m *mcpClient) tool(name string, args map[string]any) mcpContent {
	m.t.Helper()
	var r struct {
		Content []mcpContent `json:"content"`
		IsError bool         `json:"isError"`
	}
	if err := json.Unmarshal(m.rpc("tools/call", map[string]any{"name": name, "arguments": args}), &r); err != nil {
		m.t.Fatal(err)
	}
	if r.IsError || len(r.Content) == 0 {
		m.t.Fatalf("%s failed: %+v", name, r.Content)
	}
	return r.Content[0]
}

func ref(t *testing.T, snapshot, pattern string) string {
	t.Helper()
	m := regexp.MustCompile(`\[(e\d+)\] ` + pattern).FindStringSubmatch(snapshot)
	if m == nil {
		t.Fatalf("no element matching %q in snapshot:\n%s", pattern, snapshot)
	}
	return m[1]
}

func TestMCP(t *testing.T) {
	s := site()
	defer s.Close()
	base, _ := startTether(t)
	m := &mcpClient{t: t, url: base + mcp.Prefix}

	m.rpc("initialize", map[string]any{"protocolVersion": mcp.LatestProtocolVersion, "capabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "integration", "version": "1"}})
	if m.sid == "" {
		t.Fatal("no MCP session id")
	}

	// Log in, then read the page the way an agent does.
	if out := m.tool("browser_navigate", map[string]any{"url": s.URL + "/login"}).Text; !strings.Contains(out, "Title: Hello alice") {
		t.Fatalf("navigate = %s", out)
	}
	snap := m.tool("browser_snapshot", map[string]any{}).Text
	for _, want := range []string{"Page: Hello alice", "rendered-by-js", `button "Go"`, `link "again" -> /me?clicked=1`} {
		if !strings.Contains(snap, want) {
			t.Fatalf("snapshot missing %q:\n%s", want, snap)
		}
	}
	again := ref(t, snap, `link "again"`)
	if snap2 := m.tool("browser_snapshot", map[string]any{"max_text": 0}).Text; ref(t, snap2, `link "again"`) != again {
		t.Fatal("the same element got a different ref")
	}

	// Type and submit a form with real key events.
	box := ref(t, snap, `textbox "q"`)
	if out := m.tool("browser_type", map[string]any{"ref": box, "text": "tether", "submit": true}).Text; !strings.Contains(out, s.URL+"/search?q=tether") {
		t.Fatalf("type+submit = %s", out)
	}
	if out := m.tool("browser_eval", map[string]any{"expression": "document.getElementById('r').textContent"}).Text; out != `"tether"` {
		t.Fatalf("eval = %s", out)
	}
	m.tool("browser_wait_for", map[string]any{"selector": "#r", "text": "tether"})

	// Back, then click a link with a real mouse event.
	m.tool("browser_history", map[string]any{"action": "back"})
	snap = m.tool("browser_snapshot", map[string]any{}).Text
	if out := m.tool("browser_click", map[string]any{"ref": ref(t, snap, `link "again"`)}).Text; !strings.Contains(out, s.URL+"/me?clicked=1") {
		t.Fatalf("click = %s", out)
	}
	m.tool("browser_press_key", map[string]any{"key": "End"})
	m.tool("browser_scroll", map[string]any{"direction": "up"})

	// HTTP with the browser's own cookies.
	out := m.tool("browser_fetch", map[string]any{"url": s.URL + "/api/echo", "method": "POST", "body": "hi"}).Text
	if !strings.Contains(out, "HTTP 201") || !strings.Contains(out, `"sid":"alice"`) || !strings.Contains(out, `"body":"hi"`) {
		t.Fatalf("fetch = %s", out)
	}

	// Screenshots work even when the agent's background tab is idle.
	m.tool("browser_screenshot", map[string]any{})
	snap = m.tool("browser_snapshot", map[string]any{}).Text
	m.tool("browser_screenshot", map[string]any{"ref": ref(t, snap, `link "again"`)})
	shot := m.tool("browser_screenshot", map[string]any{"full_page": true})
	if raw, err := base64.StdEncoding.DecodeString(shot.Data); shot.Type != "image" || err != nil || !bytes.HasPrefix(raw, []byte("\x89PNG")) {
		t.Fatalf("screenshot is not a PNG: %s", shot.Type)
	}

	// Tabs: the agent's tab shows up, and closing the session closes it.
	tabs := m.tool("browser_tabs", map[string]any{}).Text
	if !strings.Contains(tabs, "opened by you, current") {
		t.Fatalf("tabs = %s", tabs)
	}
	req, _ := http.NewRequest("DELETE", m.url, nil)
	req.Header.Set(mcp.HeaderSessionID, m.sid)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete session: %v", err)
	}
	resp.Body.Close()
	_, body := send(t, &http.Client{}, "GET", base+"/tether/v1/tabs", nil, nil)
	if strings.Contains(body, s.URL) {
		t.Fatalf("MCP tab still open after the session ended: %s", body)
	}
}
