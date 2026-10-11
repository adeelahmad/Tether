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
	"strings"
	"testing"
	"time"

	"github.com/adeelahmad/tether/internal/app"
	"github.com/adeelahmad/tether/internal/cdp"
	"github.com/adeelahmad/tether/internal/config"
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
	mux.HandleFunc("/noisy", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!doctype html><title>noisy</title><img src="/missing.png">
<script>console.log('hello', 42, {a: 1}); console.warn('careful'); console.error('boom');</script>
<script>throw new Error('kaboom')</script>`)
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

func startTether(t *testing.T, opts ...func(*config.Config)) (base string, client *http.Client) {
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
		ConsoleBuffer: 100,
		LogLevel:      "debug", ChromeArgs: strings.Fields(os.Getenv("TETHER_CHROME_ARGS")),
	}
	for _, o := range opts {
		o(cfg)
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

type logEntry struct {
	Seq    uint64 `json:"seq"`
	Source string `json:"source"`
	Level  string `json:"level"`
	Text   string `json:"text"`
	URL    string `json:"url"`
}

// waitLogs polls the control API until the tab has logged everything want
// names (text substrings), returning the response.
func waitLogs(t *testing.T, base, tab, query string, want ...string) (entries []logEntry, next uint64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, body := send(t, http.DefaultClient, "GET", base+"/tether/v1/tabs/"+tab+"/logs"+query, nil, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("logs: %d %s", resp.StatusCode, body)
		}
		var out struct {
			Entries []logEntry `json:"entries"`
			Next    uint64     `json:"next"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		missing := ""
		for _, w := range want {
			found := false
			for _, e := range out.Entries {
				found = found || strings.Contains(e.Text, w)
			}
			if !found {
				missing = w
			}
		}
		if missing == "" {
			return out.Entries, out.Next
		}
		if time.Now().After(deadline) {
			t.Fatalf("log %q never arrived; got %+v", missing, out.Entries)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestConsoleLogs(t *testing.T) {
	s := site()
	defer s.Close()
	base, _ := startTether(t)

	resp, body := send(t, http.DefaultClient, "POST", base+"/tether/v1/tabs", strings.NewReader(`{"url":"`+s.URL+`/noisy"}`), nil)
	if resp.StatusCode != 201 {
		t.Fatalf("create tab: %d %s", resp.StatusCode, body)
	}
	tab := resp.Header.Get("X-Tether-Tab-ID")

	entries, next := waitLogs(t, base, tab, "", "hello 42", "careful", "boom", "kaboom", "404")
	bySource := map[string]logEntry{}
	for _, e := range entries {
		if _, ok := bySource[e.Source+"/"+e.Level]; !ok {
			bySource[e.Source+"/"+e.Level] = e
		}
	}
	for _, k := range []string{"console/info", "console/warning", "console/error", "exception/error", "network/error"} {
		if _, ok := bySource[k]; !ok {
			t.Errorf("no %s entry in %+v", k, entries)
		}
	}
	if e := bySource["console/info"]; !strings.HasPrefix(e.Text, "hello 42 Object") || e.URL != s.URL+"/noisy" {
		t.Errorf("console.log entry = %+v", e)
	}

	// Filtering and the cursor.
	errs, _ := waitLogs(t, base, tab, "?level=error", "boom")
	for _, e := range errs {
		if e.Level != "error" {
			t.Errorf("level filter let through %+v", e)
		}
	}
	if _, err := http.DefaultClient.Do(mustReq(t, "POST", base+"/tether/v1/tabs/"+tab+"/eval", `{"expression":"console.info('later')"}`)); err != nil {
		t.Fatal(err)
	}
	newer, _ := waitLogs(t, base, tab, fmt.Sprintf("?after=%d", next), "later")
	for _, e := range newer {
		if e.Seq <= next {
			t.Errorf("after=%d returned an older entry %+v", next, e)
		}
	}

	// The same buffer through WebDriver, attached to the tab.
	resp, err := http.Post(base+"/session", "application/json",
		strings.NewReader(`{"capabilities":{"alwaysMatch":{"tether:options":{"tabId":"`+tab+`"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	w := &wd{t: t, base: base, sid: tab}
	if types := w.call("GET", "/se/log/types", nil).([]any); len(types) == 0 || types[0] != "browser" {
		t.Fatalf("log types = %v", types)
	}
	got := w.call("POST", "/se/log", map[string]string{"type": "browser"}).([]any)
	levels := map[string]bool{}
	for _, e := range got {
		levels[e.(map[string]any)["level"].(string)] = true
	}
	if !levels["SEVERE"] || !levels["WARNING"] || !levels["INFO"] {
		t.Fatalf("webdriver log levels = %v from %v", levels, got)
	}
	// A second read returns only entries logged since the first (a late
	// favicon 404 can legitimately arrive in between), never repeats.
	seen := map[string]bool{}
	for _, e := range got {
		seen[fmt.Sprint(e)] = true
	}
	for _, e := range w.call("POST", "/log", map[string]string{"type": "browser"}).([]any) {
		if seen[fmt.Sprint(e)] {
			t.Fatalf("second read repeated entry: %v", e)
		}
	}
}

func mustReq(t *testing.T, method, u, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, u, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return req
}
