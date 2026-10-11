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
	"archive/zip"
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
	mux.HandleFunc("/frames", func(w http.ResponseWriter, r *http.Request) {
		// localhost and 127.0.0.1 are different sites, so the second frame
		// is out of process.
		cross := "http://" + strings.Replace(r.Host, "127.0.0.1", "localhost", 1)
		fmt.Fprintf(w, `<!doctype html><title>frames</title><h1 id="top">top</h1>
<div style="height:300px"></div>
<iframe id="same" src="/inner?who=same" style="border:7px solid;padding:5px" width="400" height="200"></iframe>
<iframe id="cross" src="%s/inner?who=cross" width="400" height="200"></iframe>`, cross)
	})
	mux.HandleFunc("/inner", func(w http.ResponseWriter, r *http.Request) {
		who := r.URL.Query().Get("who")
		nested := ""
		if who == "same" {
			nested = `<iframe id="nested" src="/inner?who=nested" width="300" height="80"></iframe>`
		}
		fmt.Fprintf(w, `<!doctype html><title>%s</title><h1 id="who">%s</h1>
<button id="b" onclick="this.textContent='clicked'">press</button><input id="t">%s`, who, who, nested)
	})
	mux.HandleFunc("/shadow", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!doctype html><title>shadow</title><div id="open"></div><div id="closed"></div><p id="plain">x</p>
<script>
document.getElementById('open').attachShadow({mode: 'open'}).innerHTML = '<span class="in">open inside</span><a href="#">a link</a>';
document.getElementById('closed').attachShadow({mode: 'closed'}).innerHTML = '<span class="in">closed inside</span>';
</script>`)
	})
	mux.HandleFunc("/dialogs", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!doctype html><title>dialogs</title>
<button id="alert" onclick="alert('hello'); document.getElementById('out').textContent = 'after alert'">a</button>
<button id="confirm" onclick="document.getElementById('out').textContent = 'confirm=' + confirm('sure?')">c</button>
<button id="prompt" onclick="document.getElementById('out').textContent = 'prompt=' + prompt('name?', 'def')">p</button>
<p id="out"></p>`)
	})
	mux.HandleFunc("/popups", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!doctype html><title>opener</title>
<a id="blank" href="/inner?who=blank" target="_blank">blank</a>
<button id="open" onclick="window.open('/inner?who=opened', 'w2')">open</button>`)
	})
	mux.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!doctype html><title>upload</title>
<input type="file" id="one" style="display:none" onchange="document.title = 'one:' + this.files[0].name">
<input type="file" id="many" multiple onchange="document.title = 'many:' + Array.from(this.files, f => f.name).join(',')">`)
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

// try is call for commands expected to fail; it returns the W3C error code.
func (w *wd) try(method, path string, body any) string {
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
		Value struct {
			Error string `json:"error"`
		} `json:"value"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	return out.Value.Error
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

func newWD(t *testing.T, base string, caps map[string]any) *wd {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"capabilities": map[string]any{"alwaysMatch": caps}})
	resp, err := http.Post(base+"/session", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var ns struct {
		Value struct {
			SessionID string `json:"sessionId"`
		} `json:"value"`
	}
	json.NewDecoder(resp.Body).Decode(&ns)
	if ns.Value.SessionID == "" {
		t.Fatalf("new session failed: %d", resp.StatusCode)
	}
	w := &wd{t: t, base: base, sid: ns.Value.SessionID}
	t.Cleanup(func() {
		req, _ := http.NewRequest("DELETE", base+"/session/"+w.sid, nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	})
	return w
}

func el(ref string) map[string]string { return map[string]string{webdriver.ElementKey: ref} }

func TestWebDriverFramesAndShadow(t *testing.T) {
	s := site()
	defer s.Close()
	base, _ := startTether(t)
	w := newWD(t, base, map[string]any{})

	w.call("POST", "/url", map[string]string{"url": s.URL + "/frames"})
	top := w.find("#top")

	// Same-process frame, by element.
	w.call("POST", "/frame", map[string]any{"id": el(w.find("#same"))})
	if got := w.call("GET", "/element/"+w.find("#who")+"/text", nil); got != "same" {
		t.Fatalf("same-origin frame text = %v", got)
	}
	if got := w.call("GET", "/title", nil); got != "frames" {
		t.Fatalf("title must stay the top-level one, got %v", got)
	}
	if src := w.call("GET", "/source", nil).(string); !strings.Contains(src, `id="who"`) {
		t.Fatal("page source should be the frame's")
	}
	if code := w.try("GET", "/element/"+top+"/text", nil); code != "stale element reference" && code != "no such element" {
		t.Fatalf("top element inside frame: %s", code)
	}
	btn := w.find("#b")
	w.call("POST", "/element/"+btn+"/click", map[string]any{})
	if got := w.call("GET", "/element/"+btn+"/text", nil); got != "clicked" {
		t.Fatalf("click in frame (frame offset) = %v", got)
	}
	in := w.find("#t")
	w.call("POST", "/element/"+in+"/value", map[string]string{"text": "typed"})
	if got := w.call("GET", "/element/"+in+"/property/value", nil); got != "typed" {
		t.Fatalf("send keys in frame = %v", got)
	}
	shot := w.call("GET", "/element/"+btn+"/screenshot", nil).(string)
	if raw, err := base64.StdEncoding.DecodeString(shot); err != nil || !bytes.HasPrefix(raw, []byte("\x89PNG")) {
		t.Fatal("element screenshot in frame is not a PNG")
	}

	// Nested frame by index, then back out with parent.
	w.call("POST", "/frame", map[string]any{"id": 0})
	if got := w.call("GET", "/element/"+w.find("#who")+"/text", nil); got != "nested" {
		t.Fatalf("nested frame text = %v", got)
	}
	w.call("POST", "/frame/parent", map[string]any{})
	if got := w.call("GET", "/element/"+w.find("#who")+"/text", nil); got != "same" {
		t.Fatalf("after parent = %v", got)
	}

	// Cross-site (out-of-process) frame, by index from the top.
	w.call("POST", "/frame", map[string]any{"id": nil})
	if got := w.call("GET", "/element/"+top+"/text", nil); got != "top" {
		t.Fatalf("back at top = %v", got)
	}
	w.call("POST", "/frame", map[string]any{"id": 1})
	if got := w.call("POST", "/execute/sync", map[string]any{"script": "return location.hostname + ' ' + document.getElementById('who').textContent", "args": []any{}}); got != "localhost cross" {
		t.Fatalf("cross-site frame = %v", got)
	}
	cbtn := w.find("#b")
	w.call("POST", "/element/"+cbtn+"/click", map[string]any{})
	if got := w.call("GET", "/element/"+cbtn+"/text", nil); got != "clicked" {
		t.Fatalf("click in cross-site frame = %v", got)
	}
	cin := w.find("#t")
	w.call("POST", "/element/"+cin+"/value", map[string]string{"text": "oopif"})
	if got := w.call("GET", "/element/"+cin+"/property/value", nil); got != "oopif" {
		t.Fatalf("send keys in cross-site frame = %v", got)
	}

	if code := w.try("POST", "/frame", map[string]any{"id": 9}); code != "no such frame" {
		t.Fatalf("missing frame index: %s", code)
	}

	// Navigation returns to the top-level context.
	w.call("POST", "/url", map[string]string{"url": s.URL + "/shadow"})
	if got := w.call("GET", "/element/"+w.find("#plain")+"/text", nil); got != "x" {
		t.Fatalf("after navigation = %v", got)
	}

	open := w.call("GET", "/element/"+w.find("#open")+"/shadow", nil).(map[string]any)[webdriver.ShadowKey].(string)
	span := w.call("POST", "/shadow/"+open+"/element", map[string]string{"using": "css selector", "value": ".in"}).(map[string]any)[webdriver.ElementKey].(string)
	if got := w.call("GET", "/element/"+span+"/text", nil); got != "open inside" {
		t.Fatalf("open shadow = %v", got)
	}
	links := w.call("POST", "/shadow/"+open+"/elements", map[string]string{"using": "link text", "value": "a link"}).([]any)
	if len(links) != 1 {
		t.Fatalf("link text in shadow = %v", links)
	}
	closed := w.call("GET", "/element/"+w.find("#closed")+"/shadow", nil).(map[string]any)[webdriver.ShadowKey].(string)
	span = w.call("POST", "/shadow/"+closed+"/element", map[string]string{"using": "tag name", "value": "span"}).(map[string]any)[webdriver.ElementKey].(string)
	if got := w.call("GET", "/element/"+span+"/text", nil); got != "closed inside" {
		t.Fatalf("closed shadow = %v", got)
	}
	if code := w.try("GET", "/element/"+w.find("#plain")+"/shadow", nil); code != "no such shadow root" {
		t.Fatalf("no shadow: %s", code)
	}
	got := w.call("POST", "/execute/sync", map[string]any{"script": "return arguments[0].host.id", "args": []any{map[string]string{webdriver.ShadowKey: open}}})
	if got != "open" {
		t.Fatalf("shadow root as script arg = %v", got)
	}
	ret := w.call("POST", "/execute/sync", map[string]any{"script": "return document.getElementById('open').shadowRoot", "args": []any{}}).(map[string]any)
	if ret[webdriver.ShadowKey] != open {
		t.Fatalf("shadow root returned from script = %v, want %s", ret, open)
	}
	w.call("POST", "/refresh", map[string]any{})
	if code := w.try("POST", "/shadow/"+open+"/element", map[string]string{"using": "css selector", "value": "span"}); code != "detached shadow root" {
		t.Fatalf("detached shadow: %s", code)
	}
}

func TestWebDriverAlerts(t *testing.T) {
	s := site()
	defer s.Close()
	base, _ := startTether(t)
	w := newWD(t, base, map[string]any{})
	w.call("POST", "/url", map[string]string{"url": s.URL + "/dialogs"})

	if code := w.try("GET", "/alert/text", nil); code != "no such alert" {
		t.Fatalf("no alert: %s", code)
	}
	w.call("POST", "/element/"+w.find("#alert")+"/click", map[string]any{})
	if got := w.call("GET", "/alert/text", nil); got != "hello" {
		t.Fatalf("alert text = %v", got)
	}
	if code := w.try("POST", "/alert/text", map[string]string{"text": "x"}); code != "element not interactable" {
		t.Fatalf("text into alert: %s", code)
	}
	w.call("POST", "/alert/accept", map[string]any{})
	out := w.find("#out")
	if got := w.call("GET", "/element/"+out+"/text", nil); got != "after alert" {
		t.Fatalf("after alert = %v", got)
	}

	w.call("POST", "/element/"+w.find("#confirm")+"/click", map[string]any{})
	w.call("POST", "/alert/dismiss", map[string]any{})
	if got := w.call("GET", "/element/"+out+"/text", nil); got != "confirm=false" {
		t.Fatalf("dismissed confirm = %v", got)
	}

	w.call("POST", "/element/"+w.find("#prompt")+"/click", map[string]any{})
	w.call("POST", "/alert/text", map[string]string{"text": "tether"})
	w.call("POST", "/alert/accept", map[string]any{})
	if got := w.call("GET", "/element/"+out+"/text", nil); got != "prompt=tether" {
		t.Fatalf("answered prompt = %v", got)
	}

	// Default behaviour: dismiss and notify.
	w.call("POST", "/element/"+w.find("#confirm")+"/click", map[string]any{})
	if code := w.try("GET", "/title", nil); code != "unexpected alert open" {
		t.Fatalf("command with prompt open: %s", code)
	}
	if got := w.call("GET", "/element/"+out+"/text", nil); got != "confirm=false" {
		t.Fatalf("prompt should have been dismissed: %v", got)
	}

	// A script that opens a prompt ends with null.
	if got := w.call("POST", "/execute/sync", map[string]any{"script": "alert('from script'); return 1", "args": []any{}}); got != nil {
		t.Fatalf("script opening alert = %v", got)
	}
	if got := w.call("GET", "/alert/text", nil); got != "from script" {
		t.Fatalf("alert from script = %v", got)
	}
	w.call("POST", "/alert/accept", map[string]any{})

	// accept: handled silently.
	w2 := newWD(t, base, map[string]any{"unhandledPromptBehavior": "accept"})
	w2.call("POST", "/url", map[string]string{"url": s.URL + "/dialogs"})
	w2.call("POST", "/element/"+w2.find("#confirm")+"/click", map[string]any{})
	if got := w2.call("GET", "/element/"+w2.find("#out")+"/text", nil); got != "confirm=true" {
		t.Fatalf("accept behaviour = %v", got)
	}
}

func TestWebDriverWindows(t *testing.T) {
	s := site()
	defer s.Close()
	base, _ := startTether(t)
	w := newWD(t, base, map[string]any{})
	first := w.sid
	w.call("POST", "/url", map[string]string{"url": s.URL + "/popups"})

	w.call("POST", "/element/"+w.find("#blank")+"/click", map[string]any{})
	w.call("POST", "/element/"+w.find("#open")+"/click", map[string]any{})
	var handles []any
	for i := 0; i < 50; i++ {
		handles = w.call("GET", "/window/handles", nil).([]any)
		if len(handles) == 3 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(handles) != 3 || handles[0] != first {
		t.Fatalf("handles after popups = %v", handles)
	}
	seen := map[string]bool{}
	for _, h := range handles[1:] {
		w.call("POST", "/window", map[string]any{"handle": h})
		if got := w.call("GET", "/window", nil); got != h {
			t.Fatalf("current window = %v, want %v", got, h)
		}
		for i := 0; i < 50; i++ {
			if w.call("GET", "/title", nil) != "" {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		seen[w.call("GET", "/title", nil).(string)] = true
	}
	if !seen["blank"] || !seen["opened"] {
		t.Fatalf("popup titles = %v", seen)
	}

	// Close the current popup: no current window until switching.
	left := w.call("DELETE", "/window", nil).([]any)
	if len(left) != 2 {
		t.Fatalf("after close = %v", left)
	}
	if code := w.try("GET", "/title", nil); code != "no such window" {
		t.Fatalf("closed current window: %s", code)
	}
	if code := w.try("POST", "/window", map[string]any{"handle": "nope"}); code != "no such window" {
		t.Fatalf("bogus handle: %s", code)
	}
	w.call("POST", "/window", map[string]any{"handle": first})
	if got := w.call("GET", "/title", nil); got != "opener" {
		t.Fatalf("back at first = %v", got)
	}

	// New tab, and a new window whose geometry this session controls.
	nt := w.call("POST", "/window/new", map[string]string{"type": "tab"}).(map[string]any)
	if nt["type"] != "tab" || nt["handle"] == "" {
		t.Fatalf("new tab = %v", nt)
	}
	nw := w.call("POST", "/window/new", map[string]string{"type": "window"}).(map[string]any)
	if nw["type"] != "window" {
		t.Fatalf("new window = %v", nw)
	}
	if n := len(w.call("GET", "/window/handles", nil).([]any)); n != 4 {
		t.Fatalf("handles = %d, want 4", n)
	}
	w.call("POST", "/window", map[string]any{"handle": nw["handle"]})
	r := w.call("POST", "/window/rect", map[string]any{"x": 20, "y": 30, "width": 700, "height": 500}).(map[string]any)
	if r["width"] != float64(700) || r["height"] != float64(500) {
		t.Fatalf("resized rect = %v", r)
	}
	if got := w.call("GET", "/window/rect", nil).(map[string]any); got["width"] != float64(700) {
		t.Fatalf("rect = %v", got)
	}
	w.call("POST", "/window/maximize", map[string]any{})
	w.call("POST", "/window/fullscreen", map[string]any{})
	w.call("POST", "/window/minimize", map[string]any{})
	r = w.call("POST", "/window/rect", map[string]any{"width": 640, "height": 480}).(map[string]any)
	if r["width"] != float64(640) {
		t.Fatalf("rect after restoring = %v", r)
	}
	if code := w.try("POST", "/window/rect", map[string]any{"width": -1}); code != "invalid argument" {
		t.Fatalf("negative width: %s", code)
	}

	// The first window shares Chrome's window with a tab that isn't the
	// session's, so its geometry is left alone.
	w.call("POST", "/window", map[string]any{"handle": first})
	before := w.call("GET", "/window/rect", nil).(map[string]any)
	after := w.call("POST", "/window/rect", map[string]any{"width": 333, "height": 333}).(map[string]any)
	if after["width"] != before["width"] {
		t.Fatalf("shared window was resized: %v -> %v", before, after)
	}
}

func TestWebDriverUploadAndPrint(t *testing.T) {
	s := site()
	defer s.Close()
	base, _ := startTether(t)
	w := newWD(t, base, map[string]any{})
	w.call("POST", "/url", map[string]string{"url": s.URL + "/upload"})

	dir := t.TempDir()
	local := dir + "/local.txt"
	if err := os.WriteFile(local, []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.call("POST", "/element/"+w.find("#one")+"/value", map[string]string{"text": local})
	if got := w.call("GET", "/title", nil); got != "one:local.txt" {
		t.Fatalf("local upload = %v", got)
	}

	// Selenium's remote file upload: a zipped file, then its server path.
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	f, _ := zw.Create("remote.txt")
	f.Write([]byte("remote"))
	zw.Close()
	path := w.call("POST", "/se/file", map[string]string{"file": base64.StdEncoding.EncodeToString(zbuf.Bytes())}).(string)
	if b, err := os.ReadFile(path); err != nil || string(b) != "remote" {
		t.Fatalf("uploaded file %s: %q %v", path, b, err)
	}
	w.call("POST", "/element/"+w.find("#many")+"/value", map[string]string{"text": local + "\n" + path})
	if got := w.call("GET", "/title", nil); got != "many:local.txt,remote.txt" {
		t.Fatalf("multiple upload = %v", got)
	}
	if code := w.try("POST", "/element/"+w.find("#one")+"/value", map[string]string{"text": local + "\n" + path}); code != "invalid argument" {
		t.Fatalf("two files into a single input: %s", code)
	}
	if code := w.try("POST", "/element/"+w.find("#one")+"/value", map[string]string{"text": dir + "/missing"}); code != "invalid argument" {
		t.Fatalf("missing file: %s", code)
	}

	pdf := w.call("POST", "/print", map[string]any{"orientation": "landscape", "background": true, "pageRanges": []any{1}}).(string)
	if raw, err := base64.StdEncoding.DecodeString(pdf); err != nil || !bytes.HasPrefix(raw, []byte("%PDF")) {
		t.Fatal("print did not return a PDF")
	}
	if code := w.try("POST", "/print", map[string]any{"scale": 5}); code != "invalid argument" {
		t.Fatalf("bad scale: %s", code)
	}

	req, _ := http.NewRequest("DELETE", base+"/session/"+w.sid, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("uploaded file should be removed with the session: %v", err)
	}
}
