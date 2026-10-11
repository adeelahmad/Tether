package remote

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/adeelahmad/tether/internal/cdp"
	"github.com/adeelahmad/tether/internal/cdp/cdptest"
	"github.com/adeelahmad/tether/internal/core"
	"github.com/adeelahmad/tether/internal/session"
)

type fixture struct {
	b    *cdptest.Browser
	core *core.Core
	ui   *UI
	srv  *httptest.Server
}

func newFixture(t *testing.T, policy core.Policy) *fixture {
	t.Helper()
	b := cdptest.NewBrowser()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := session.NewManager(b, session.Options{Logger: quiet})
	c := core.New(b, m, core.Options{Policy: policy, Timeout: 5 * time.Second, Logger: quiet})
	ui := New(c, Options{})
	srv := httptest.NewServer(ui)
	t.Cleanup(func() {
		ui.Close()
		srv.Close()
	})
	return &fixture{b: b, core: c, ui: ui, srv: srv}
}

func (f *fixture) do(t *testing.T, method, path, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, f.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestStaticAndHeaders(t *testing.T) {
	f := newFixture(t, nil)
	tests := []struct {
		path       string
		wantStatus int
		wantType   string
		wantBody   string
	}{
		{"/remote/", 200, "text/html", `<canvas id="screen"`},
		{"/remote/static/app.js", 200, "javascript", "new WebSocket"},
		{"/remote/static/app.css", 200, "text/css", "--accent"},
		{"/remote/static/icon.svg", 200, "image/svg+xml", "<svg"},
		{"/remote", 301, "", ""},
		{"/remote/nope", 404, "application/json", "not_found"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			resp, body := f.do(t, "GET", tt.path, "")
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if !strings.Contains(resp.Header.Get("Content-Type"), tt.wantType) {
				t.Errorf("content-type = %q", resp.Header.Get("Content-Type"))
			}
			if !strings.Contains(body, tt.wantBody) {
				t.Errorf("body lacks %q", tt.wantBody)
			}
			csp := resp.Header.Get("Content-Security-Policy")
			if !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "script-src 'self'") {
				t.Errorf("CSP = %q", csp)
			}
			if resp.Header.Get("X-Frame-Options") != "DENY" {
				t.Error("missing X-Frame-Options")
			}
		})
	}
}

func TestListTabs(t *testing.T) {
	f := newFixture(t, nil)
	f.b.AddTab("USER1", "https://mail.example.com/")
	s, err := f.core.Sessions.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	resp, body := f.do(t, "GET", "/remote/api/tabs", "")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out struct{ Tabs []tabView }
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	got := map[string]tabView{}
	for _, v := range out.Tabs {
		got[v.ID] = v
	}
	if u := got["USER1"]; u.URL != "https://mail.example.com/" || u.Session || u.Owned {
		t.Errorf("user tab = %+v", u)
	}
	if o := got[s.ID]; !o.Session || !o.Owned {
		t.Errorf("owned tab = %+v", o)
	}
}

func TestCreateTab(t *testing.T) {
	f := newFixture(t, core.DomainAllowlist([]string{"example.com"}))

	resp, body := f.do(t, "POST", "/remote/api/tabs", `{"url":"example.com/inbox"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	id := resp.Header.Get("X-Tether-Tab-ID")
	s, err := f.core.Sessions.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if nav := s.Tab.(*cdptest.Tab).Navigations; len(nav) != 1 || nav[0] != "https://example.com/inbox" {
		t.Fatalf("navigations = %v", nav)
	}

	// Policy applies, and a refused tab is not left behind.
	before := len(f.b.Open())
	resp, _ = f.do(t, "POST", "/remote/api/tabs", `{"url":"https://evil.test/"}`)
	if resp.StatusCode != 403 {
		t.Fatalf("forbidden url: status %d", resp.StatusCode)
	}
	if len(f.b.Open()) != before {
		t.Fatal("refused tab was left open")
	}

	resp, _ = f.do(t, "POST", "/remote/api/tabs", `{`)
	if resp.StatusCode != 400 {
		t.Fatalf("bad json: status %d", resp.StatusCode)
	}
	resp, _ = f.do(t, "POST", "/remote/api/tabs", ``)
	if resp.StatusCode != 201 {
		t.Fatalf("blank tab: status %d", resp.StatusCode)
	}
}

func TestCloseTab(t *testing.T) {
	f := newFixture(t, nil)
	f.b.AddTab("MINE", "https://example.com/")
	s, _ := f.core.Sessions.Create(context.Background())

	if resp, _ := f.do(t, "DELETE", "/remote/api/tabs/"+s.ID, ""); resp.StatusCode != 204 {
		t.Fatalf("owned close: %d", resp.StatusCode)
	}
	if _, err := f.b.AttachTab(context.Background(), s.ID); err == nil {
		t.Fatal("owned tab still open in Chrome")
	}
	// A user's tab is only released without force...
	if resp, _ := f.do(t, "DELETE", "/remote/api/tabs/MINE", ""); resp.StatusCode != 204 {
		t.Fatalf("release: %d", resp.StatusCode)
	}
	if _, err := f.b.AttachTab(context.Background(), "MINE"); err != nil {
		t.Fatal("user tab was closed without force")
	}
	// ...and closed with it.
	if resp, _ := f.do(t, "DELETE", "/remote/api/tabs/MINE?force=true", ""); resp.StatusCode != 204 {
		t.Fatalf("force close: %d", resp.StatusCode)
	}
	if _, err := f.b.AttachTab(context.Background(), "MINE"); err == nil {
		t.Fatal("forced close left the tab open")
	}
	if resp, _ := f.do(t, "DELETE", "/remote/api/tabs/NOPE", ""); resp.StatusCode != 410 {
		t.Fatalf("unknown tab: %d", resp.StatusCode)
	}
}

func TestInfo(t *testing.T) {
	f := newFixture(t, nil)
	_, body := f.do(t, "GET", "/remote/api/info", "")
	if !strings.Contains(body, "FakeChrome") || !strings.Contains(body, `"auth": false`) {
		t.Fatalf("info = %s", body)
	}
}

// viewerClient is a websocket client of /remote/ws/{id}.
type viewerClient struct {
	t *testing.T
	c *websocket.Conn
}

func (f *fixture) watch(t *testing.T, id string) *viewerClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(f.srv.URL, "http")+"/remote/ws/"+id, nil)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return &viewerClient{t: t, c: c}
}

func (v *viewerClient) send(m map[string]any) {
	v.t.Helper()
	b, _ := json.Marshal(m)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := v.c.Write(ctx, websocket.MessageText, b); err != nil {
		v.t.Fatal(err)
	}
}

// next returns the next text message of kind t, skipping frames and other
// messages.
func (v *viewerClient) next(t string) serverMsg {
	v.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		typ, b, err := v.c.Read(ctx)
		if err != nil {
			v.t.Fatalf("waiting for %q: %v", t, err)
		}
		if typ != websocket.MessageText {
			continue
		}
		var m serverMsg
		if err := json.Unmarshal(b, &m); err != nil {
			v.t.Fatal(err)
		}
		if m.T == t {
			return m
		}
	}
}

func (v *viewerClient) frame() (cdp.FrameMeta, []byte) {
	v.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		typ, b, err := v.c.Read(ctx)
		if err != nil {
			v.t.Fatal(err)
		}
		if typ != websocket.MessageBinary {
			continue
		}
		n := binary.BigEndian.Uint32(b)
		var meta cdp.FrameMeta
		if err := json.Unmarshal(b[4:4+n], &meta); err != nil {
			v.t.Fatal(err)
		}
		return meta, b[4+n:]
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for " + what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestViewerStreamsAndDrives(t *testing.T) {
	f := newFixture(t, core.DomainAllowlist([]string{"example.com"}))
	tab := f.b.AddTab("T1", "https://example.com/")
	tab.EvalResult = json.RawMessage(`"selected words"`)
	v := f.watch(t, "T1")

	// Tab info comes first, then frames.
	if m := v.next("info"); m.URL != "https://example.com/" {
		t.Fatalf("info = %+v", m)
	}
	meta, jpeg := v.frame()
	if meta.DeviceWidth != 800 || meta.DeviceHeight != 600 || !bytes.Equal(jpeg, cdptest.FakeJPEG) {
		t.Fatalf("frame = %+v %q", meta, jpeg)
	}

	v.send(map[string]any{"t": "mouse", "type": "mousePressed", "x": 10.5, "y": 20, "button": "left", "buttons": 1, "clickCount": 1})
	v.send(map[string]any{"t": "wheel", "x": 1, "y": 2, "deltaY": 120})
	v.send(map[string]any{"t": "key", "type": "keyDown", "key": "a", "code": "KeyA", "keyCode": 65, "text": "a"})
	v.send(map[string]any{"t": "key", "type": "keyDown", "key": "ArrowLeft", "code": "ArrowLeft", "keyCode": 37})
	v.send(map[string]any{"t": "key", "type": "keyDown", "key": "a", "modifiers": 2, "commands": []string{"selectAll"}})
	v.send(map[string]any{"t": "text", "text": "pasted ✓"})
	v.send(map[string]any{"t": "front"})

	waitFor(t, "input", func() bool {
		keys, mice, texts := tab.Input()
		return len(keys) == 3 && len(mice) == 2 && len(texts) == 1
	})
	keys, mice, texts := tab.Input()
	if mice[0].Type != "mousePressed" || mice[0].X != 10.5 || mice[0].Button != "left" || mice[0].ClickCount != 1 {
		t.Errorf("mouse = %+v", mice[0])
	}
	if mice[1].Type != "mouseWheel" || mice[1].DeltaY != 120 {
		t.Errorf("wheel = %+v", mice[1])
	}
	if keys[0].Type != "keyDown" || keys[0].Text != "a" {
		t.Errorf("printable key = %+v", keys[0])
	}
	if keys[1].Type != "rawKeyDown" || keys[1].Text != "" {
		t.Errorf("non-printable key = %+v", keys[1])
	}
	if len(keys[2].Commands) != 1 || keys[2].Commands[0] != "selectAll" || keys[2].Modifiers != 2 {
		t.Errorf("shortcut = %+v", keys[2])
	}
	if texts[0] != "pasted ✓" {
		t.Errorf("text = %q", texts[0])
	}

	v.send(map[string]any{"t": "copy"})
	if m := v.next("clipboard"); m.Text != "selected words" {
		t.Errorf("clipboard = %+v", m)
	}

	// Bad input is reported, not fatal.
	v.send(map[string]any{"t": "mouse", "type": "teleport"})
	if m := v.next("error"); m.Code != core.CodeBadRequest {
		t.Errorf("error = %+v", m)
	}
	v.send(map[string]any{"t": "key", "type": "keyDown", "commands": []string{"delete"}})
	if m := v.next("error"); !strings.Contains(m.Message, "not allowed") {
		t.Errorf("error = %+v", m)
	}

	// Navigation goes through the core and its policy.
	v.send(map[string]any{"t": "nav", "url": "example.com/next"})
	waitFor(t, "navigation", func() bool {
		info, _ := tab.Info(context.Background())
		return info.URL == "https://example.com/next"
	})
	v.send(map[string]any{"t": "nav", "url": "https://evil.test/"})
	if m := v.next("error"); m.Code != core.CodeForbidden {
		t.Errorf("forbidden nav = %+v", m)
	}
	if tab.Fronted != 1 {
		t.Errorf("fronted = %d", tab.Fronted)
	}
}

func TestViewersShareOneScreencast(t *testing.T) {
	f := newFixture(t, nil)
	tab := f.b.AddTab("T1", "about:blank")
	a := f.watch(t, "T1")
	b := f.watch(t, "T1")
	a.frame()
	b.frame()
	if n := tab.Casting(); n != 1 {
		t.Fatalf("screencasts = %d, want 1", n)
	}
	a.c.Close(websocket.StatusNormalClosure, "")
	b.frame() // b keeps streaming
	b.c.Close(websocket.StatusNormalClosure, "")
	waitFor(t, "screencast to stop", func() bool { return tab.Casting() == 0 && f.ui.hub.active() == 0 })
}

func TestViewerSeesTabClose(t *testing.T) {
	f := newFixture(t, nil)
	f.b.AddTab("T1", "about:blank")
	v := f.watch(t, "T1")
	v.frame()
	f.b.Destroy("T1")
	if m := v.next("ended"); m.Code != "tab_gone" {
		t.Fatalf("ended = %+v", m)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if _, _, err := v.c.Read(ctx); err != nil {
			if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
				t.Fatalf("close = %v", err)
			}
			break
		}
	}
}

func TestCloseEndsViewers(t *testing.T) {
	f := newFixture(t, nil)
	tab := f.b.AddTab("T1", "about:blank")
	v := f.watch(t, "T1")
	v.frame()
	f.ui.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if _, _, err := v.c.Read(ctx); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("viewer not disconnected on Close")
			}
			break
		}
	}
	waitFor(t, "screencast to stop", func() bool { return tab.Casting() == 0 })
}

func TestWSRejections(t *testing.T) {
	f := newFixture(t, nil)
	f.b.AddTab("T1", "about:blank")

	resp, _ := f.do(t, "GET", "/remote/ws/NOPE", "")
	if resp.StatusCode != 410 {
		t.Fatalf("unknown tab: %d", resp.StatusCode)
	}

	// Another site cannot open a socket to a tab.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(f.srv.URL, "http")+"/remote/ws/T1",
		&websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://evil.example"}}})
	if err == nil {
		c.CloseNow()
		t.Fatal("cross-origin websocket accepted")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin: %v", resp)
	}

	// Behind a reverse proxy the forwarded host is the page's origin.
	c, resp, err = websocket.Dial(ctx, "ws"+strings.TrimPrefix(f.srv.URL, "http")+"/remote/ws/T1",
		&websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://box.tail1234.ts.net"}, "X-Forwarded-Host": {"box.tail1234.ts.net"}}})
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("forwarded origin refused: %v", err)
	}
	c.CloseNow()
}

func TestNormalizeURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", "about:blank"},
		{"https://example.com/a?b=c", "https://example.com/a?b=c"},
		{"http://localhost:3000", "http://localhost:3000"},
		{"example.com", "https://example.com"},
		{"  github.com/settings ", "https://github.com/settings"},
		{"example.com:8443/x", "https://example.com:8443/x"},
		{"localhost:8080", "http://localhost:8080"},
		{"127.0.0.1:9222/json", "http://127.0.0.1:9222/json"},
		{"about:blank", "about:blank"},
		{"chrome://version", "chrome://version"},
		{"what is tether", "https://www.google.com/search?q=what+is+tether"},
		{"intranet", "https://www.google.com/search?q=intranet"},
	}
	for _, tt := range tests {
		if got := normalizeURL(tt.in); got != tt.want {
			t.Errorf("normalizeURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestKeyEvent(t *testing.T) {
	tests := []struct {
		name    string
		in      clientMsg
		want    cdp.KeyEvent
		wantErr bool
	}{
		{"printable", clientMsg{Type: "keyDown", Key: "x", Text: "x"}, cdp.KeyEvent{Type: "keyDown", Key: "x", Text: "x"}, false},
		{"enter", clientMsg{Type: "keyDown", Key: "Enter", KeyCode: 13, Text: "\r"}, cdp.KeyEvent{Type: "keyDown", Key: "Enter", KeyCode: 13, Text: "\r"}, false},
		{"raw", clientMsg{Type: "keyDown", Key: "Tab"}, cdp.KeyEvent{Type: "rawKeyDown", Key: "Tab"}, false},
		{"up drops text", clientMsg{Type: "keyUp", Key: "x", Text: "x"}, cdp.KeyEvent{Type: "keyUp", Key: "x"}, false},
		{"bad type", clientMsg{Type: "explode"}, cdp.KeyEvent{}, true},
		{"long text", clientMsg{Type: "keyDown", Text: strings.Repeat("a", 17)}, cdp.KeyEvent{}, true},
		{"bad command", clientMsg{Type: "keyDown", Commands: []string{"deleteEverything"}}, cdp.KeyEvent{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := keyEvent(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v", err)
			}
			if !tt.wantErr && (got.Type != tt.want.Type || got.Text != tt.want.Text || got.Key != tt.want.Key) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestEncodeFrame(t *testing.T) {
	b, err := encodeFrame(cdp.Frame{Data: []byte("JPEG"), Meta: cdp.FrameMeta{DeviceWidth: 1}})
	if err != nil {
		t.Fatal(err)
	}
	n := binary.BigEndian.Uint32(b)
	if !bytes.HasPrefix(b[4:], []byte(`{"deviceWidth":1`)) || string(b[4+n:]) != "JPEG" {
		t.Fatalf("frame = %q", b)
	}
}

func TestDialogsAreRelayed(t *testing.T) {
	f := newFixture(t, nil)
	tab := f.b.AddTab("T1", "https://example.com/")
	v := f.watch(t, "T1")
	v.next("info") // the socket is up and watching

	tab.OpenDialog(cdp.Dialog{Type: "prompt", Message: "Your name?", DefaultPrompt: "anon", URL: "https://example.com/"})
	m := v.next("dialog")
	if m.Dialog == nil || m.Dialog.Type != "prompt" || m.Dialog.Message != "Your name?" || m.Dialog.DefaultPrompt != "anon" {
		t.Fatalf("dialog = %+v", m.Dialog)
	}
	v.send(map[string]any{"t": "dialog", "accept": true, "text": "Ada"})
	if m := v.next("dialog"); m.Dialog == nil || !m.Dialog.Closed {
		t.Fatalf("close = %+v", m.Dialog)
	}
	if a := tab.Answers(); len(a) != 1 || !a[0].Accept || a[0].Text != "Ada" {
		t.Fatalf("answers = %+v", a)
	}
	// Answering when nothing is open is reported, not fatal.
	v.send(map[string]any{"t": "dialog", "accept": false})
	if m := v.next("error"); !strings.Contains(m.Message, "no dialog") {
		t.Fatalf("error = %+v", m)
	}
}

func TestQuickReconnectWaitsForOldScreencast(t *testing.T) {
	f := newFixture(t, nil)
	tab := f.b.AddTab("T1", "about:blank")
	for i := 0; i < 5; i++ {
		v := f.watch(t, "T1")
		v.frame()
		v.c.Close(websocket.StatusNormalClosure, "")
	}
	v := f.watch(t, "T1")
	v.frame()
	waitFor(t, "one screencast", func() bool { return tab.Casting() == 1 })
	time.Sleep(50 * time.Millisecond)
	if n := tab.Casting(); n != 1 {
		t.Fatalf("overlapping screencasts: %d", n)
	}
}
