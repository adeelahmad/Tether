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
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<!doctype html><title>results</title><h1 id="r">%s</h1>`, r.URL.Query().Get("q"))
	})
	mux.HandleFunc("/actions", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!doctype html><title>actions</title>
<style>body{margin:0;height:4000px} .box{position:absolute;width:100px;height:100px}</style>
<div id="a" class="box" style="left:20px;top:20px;background:red"></div>
<div id="b" class="box" style="left:300px;top:20px;background:blue"></div>
<input id="t" style="position:absolute;left:20px;top:200px">
<div id="far" class="box" style="left:20px;top:3000px;background:green"></div>
<script>
window.log = [];
const rec = (e) => log.push([e.type, e.target.id || e.target.tagName, e.key || '',
  e.detail || 0, e.ctrlKey ? 'ctrl' : '', e.shiftKey ? 'shift' : ''].join(':'));
for (const t of ['mousedown','mouseup','click','dblclick','contextmenu','keydown','keyup','touchstart','touchend'])
  addEventListener(t, rec, true);
addEventListener('contextmenu', (e) => e.preventDefault());
</script>`)
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
		LogLevel: "debug", ChromeArgs: strings.Fields(os.Getenv("TETHER_CHROME_ARGS")),
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

func TestWebDriverActions(t *testing.T) {
	s := site()
	defer s.Close()
	base, _ := startTether(t)

	resp, err := http.Post(base+"/session", "application/json",
		strings.NewReader(`{"capabilities":{"alwaysMatch":{"browserName":"chrome","timeouts":{"pageLoad":20000}}}}`))
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
	defer func() {
		req, _ := http.NewRequest("DELETE", base+"/session/"+w.sid, nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	w.call("POST", "/url", map[string]string{"url": s.URL + "/actions"})

	el := func(css string) map[string]string { return map[string]string{webdriver.ElementKey: w.find(css)} }
	a, b := el("#a"), el("#b")
	mouse := func(acts ...any) map[string]any {
		return map[string]any{"type": "pointer", "id": "mouse", "parameters": map[string]string{"pointerType": "mouse"}, "actions": acts}
	}
	keys := func(acts ...any) map[string]any { return map[string]any{"type": "key", "id": "kb", "actions": acts} }
	moveTo := func(origin any) map[string]any {
		return map[string]any{"type": "pointerMove", "origin": origin, "x": 0, "y": 0}
	}
	down := func(btn int) map[string]any { return map[string]any{"type": "pointerDown", "button": btn} }
	up := func(btn int) map[string]any { return map[string]any{"type": "pointerUp", "button": btn} }
	kd := func(v string) map[string]any { return map[string]any{"type": "keyDown", "value": v} }
	ku := func(v string) map[string]any { return map[string]any{"type": "keyUp", "value": v} }
	pause := map[string]any{"type": "pause", "duration": 0}
	perform := func(seqs ...any) { w.call("POST", "/actions", map[string]any{"actions": seqs}) }
	events := func() string {
		v := w.call("POST", "/execute/sync", map[string]any{"script": "const l = log; window.log = []; return l.join(' ');", "args": []any{}})
		return v.(string)
	}
	expect := func(what, got string, want ...string) {
		t.Helper()
		for _, s := range want {
			if !strings.Contains(got, s) {
				t.Fatalf("%s: missing %q in %s", what, s, got)
			}
		}
	}

	perform(mouse(moveTo(a), down(0), up(0), down(0), up(0)))
	expect("double click", events(), "click:a::1::", "click:a::2::", "dblclick:a::2::")

	perform(mouse(moveTo(b), down(2), up(2)))
	expect("context click", events(), "contextmenu:b:")

	perform(keys(kd("\uE009"), pause, pause, ku("\uE009")), mouse(moveTo(a), down(0), up(0)))
	expect("ctrl click", events(), "click:a::1:ctrl:")

	perform(mouse(moveTo(a), down(0), map[string]any{"type": "pointerMove", "origin": b, "x": 0, "y": 0, "duration": 100}, up(0)))
	expect("drag", events(), "mousedown:a:", "mouseup:b:")

	in := el("#t")
	perform(mouse(moveTo(in), down(0), up(0)))
	perform(keys(kd("\uE008"), kd("h"), ku("h"), kd("i"), ku("i"), ku("\uE008"), kd("1"), ku("1")))
	if got := w.call("GET", "/element/"+in[webdriver.ElementKey]+"/property/value", nil); got != "HI1" {
		t.Fatalf("typed = %v", got)
	}
	expect("keys", events(), "keydown:t:H:0::shift", "keydown:t:1:0::")

	perform(map[string]any{"type": "pointer", "id": "finger", "parameters": map[string]string{"pointerType": "touch"},
		"actions": []any{moveTo(a), down(0), up(0)}})
	expect("tap", events(), "touchstart:a:", "touchend:a:")

	// An element below the fold is scrolled into view to be reached.
	perform(mouse(moveTo(el("#far")), down(0), up(0)))
	expect("far click", events(), "click:far:")

	perform(map[string]any{"type": "wheel", "id": "wheel", "actions": []any{
		map[string]any{"type": "scroll", "x": 10, "y": 10, "deltaX": 0, "deltaY": -2000},
	}})
	var y float64
	for i := 0; i < 50; i++ {
		y = w.call("POST", "/execute/sync", map[string]any{"script": "return scrollY", "args": []any{}}).(float64)
		if y < 2000 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if y >= 2000 {
		t.Fatalf("wheel did not scroll up: scrollY = %v", y)
	}

	// Out of the viewport is a W3C error, not a silent no-op.
	body, _ := json.Marshal(map[string]any{"actions": []any{mouse(map[string]any{"type": "pointerMove", "x": -5, "y": 0})}})
	resp, err = http.Post(base+"/session/"+w.sid+"/actions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 500 || !strings.Contains(string(out), "move target out of bounds") {
		t.Fatalf("out of bounds: %d %s", resp.StatusCode, out)
	}

	// Release lets go of keys still held.
	perform(keys(kd("\uE008")))
	events()
	w.call("DELETE", "/actions", nil)
	expect("release", events(), "keyup:", ":Shift:")
}
