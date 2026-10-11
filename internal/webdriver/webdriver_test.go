package webdriver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/adeelahmad/tether/internal/cdp/cdptest"
	"github.com/adeelahmad/tether/internal/core"
	"github.com/adeelahmad/tether/internal/session"
)

func newHandler(t *testing.T) (*Handler, *cdptest.Browser, *core.Core) {
	t.Helper()
	b := cdptest.NewBrowser()
	c := core.New(b, session.NewManager(b, session.Options{}), core.Options{Timeout: 5 * time.Second})
	return New(c, nil), b, c
}

func do(t *testing.T, h http.Handler, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var r *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		r = httptest.NewRequest(method, path, bytes.NewReader(b))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s: non-JSON body %q", method, path, w.Body.String())
	}
	return w.Code, out
}

func errCode(out map[string]any) string {
	v, _ := out["value"].(map[string]any)
	s, _ := v["error"].(string)
	return s
}

func newSession(t *testing.T, h http.Handler, caps map[string]any) string {
	t.Helper()
	code, out := do(t, h, "POST", "/session", map[string]any{"capabilities": map[string]any{"alwaysMatch": caps}})
	if code != 200 {
		t.Fatalf("new session: %d %v", code, out)
	}
	return out["value"].(map[string]any)["sessionId"].(string)
}

func TestStatus(t *testing.T) {
	h, _, _ := newHandler(t)
	for _, p := range []string{"/status", "/wd/hub/status"} {
		code, out := do(t, h, "GET", p, nil)
		if code != 200 || out["value"].(map[string]any)["ready"] != true {
			t.Fatalf("%s: %d %v", p, code, out)
		}
	}
}

func TestSessionLifecycle(t *testing.T) {
	h, b, _ := newHandler(t)
	sid := newSession(t, h, map[string]any{"browserName": "chrome", "pageLoadStrategy": "eager"})
	if len(b.Open()) != 1 || b.Open()[0] != sid {
		t.Fatalf("session id should be the tab id; open=%v sid=%s", b.Open(), sid)
	}
	code, out := do(t, h, "GET", "/session/"+sid+"/window", nil)
	if code != 200 || out["value"] != sid {
		t.Fatalf("window handle: %v", out)
	}
	code, out = do(t, h, "POST", "/session/"+sid+"/url", map[string]any{"url": "https://example.com/"})
	if code != 200 {
		t.Fatalf("navigate: %d %v", code, out)
	}
	_, out = do(t, h, "GET", "/session/"+sid+"/url", nil)
	if out["value"] != "https://example.com/" {
		t.Fatalf("url: %v", out)
	}
	if code, _ := do(t, h, "DELETE", "/session/"+sid, nil); code != 200 {
		t.Fatal("delete failed")
	}
	if len(b.Open()) != 0 {
		t.Fatal("tab not closed on delete")
	}
	code, out = do(t, h, "GET", "/session/"+sid+"/url", nil)
	if code != 404 || errCode(out) != ErrInvalidSessionID {
		t.Fatalf("after delete: %d %v", code, out)
	}
}

func TestAttachToExistingTabKeepsIt(t *testing.T) {
	h, b, _ := newHandler(t)
	b.AddTab("USER", "https://mail.example.com/")
	sid := newSession(t, h, map[string]any{"tether:options": map[string]any{"tabId": "USER"}})
	if sid != "USER" {
		t.Fatalf("sid = %s", sid)
	}
	code, out := do(t, h, "POST", "/session", map[string]any{"capabilities": map[string]any{"alwaysMatch": map[string]any{"tether:options": map[string]any{"tabId": "USER"}}}})
	if code != 500 || errCode(out) != ErrSessionNotCreated {
		t.Fatalf("double attach: %d %v", code, out)
	}
	do(t, h, "DELETE", "/session/USER", nil)
	if len(b.Open()) != 1 {
		t.Fatal("attached tab was closed on delete")
	}
}

func TestCapabilityErrors(t *testing.T) {
	h, _, _ := newHandler(t)
	tests := []struct {
		name string
		body any
		code string
	}{
		{"firefox", map[string]any{"capabilities": map[string]any{"alwaysMatch": map[string]any{"browserName": "firefox"}}}, ErrSessionNotCreated},
		{"dup", map[string]any{"capabilities": map[string]any{"alwaysMatch": map[string]any{"a": 1}, "firstMatch": []any{map[string]any{"a": 2}}}}, ErrInvalidArgument},
		{"bad strategy", map[string]any{"capabilities": map[string]any{"alwaysMatch": map[string]any{"pageLoadStrategy": "slow"}}}, ErrInvalidArgument},
		{"bad timeouts", map[string]any{"capabilities": map[string]any{"alwaysMatch": map[string]any{"timeouts": map[string]any{"implicit": -1}}}}, ErrInvalidArgument},
		{"missing tab", map[string]any{"capabilities": map[string]any{"alwaysMatch": map[string]any{"tether:options": map[string]any{"tabId": "NOPE"}}}}, ErrSessionNotCreated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, out := do(t, h, "POST", "/session", tt.body)
			if errCode(out) != tt.code {
				t.Fatalf("got %v, want %s", out, tt.code)
			}
		})
	}
}

func TestTimeouts(t *testing.T) {
	h, _, _ := newHandler(t)
	sid := newSession(t, h, nil)
	if code, out := do(t, h, "POST", "/session/"+sid+"/timeouts", map[string]any{"implicit": 250, "script": nil}); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	_, out := do(t, h, "GET", "/session/"+sid+"/timeouts", nil)
	v := out["value"].(map[string]any)
	if v["implicit"] != float64(250) || v["script"] != nil || v["pageLoad"] != float64(300000) {
		t.Fatalf("timeouts = %v", v)
	}
	if _, out := do(t, h, "POST", "/session/"+sid+"/timeouts", map[string]any{"bogus": 1}); errCode(out) != ErrInvalidArgument {
		t.Fatalf("unknown timeout accepted: %v", out)
	}
}

func TestUnknownCommandAndWindows(t *testing.T) {
	h, _, _ := newHandler(t)
	code, out := do(t, h, "POST", "/session/x/actions", map[string]any{})
	if code != 404 || errCode(out) != ErrUnknownCommand {
		t.Fatalf("%d %v", code, out)
	}
	sid := newSession(t, h, nil)
	_, out = do(t, h, "POST", "/session/"+sid+"/window", map[string]any{"handle": "OTHER"})
	if errCode(out) != ErrNoSuchWindow {
		t.Fatalf("switch: %v", out)
	}
	_, out = do(t, h, "GET", "/session/"+sid+"/window/handles", nil)
	if hs := out["value"].([]any); len(hs) != 1 || hs[0] != sid {
		t.Fatalf("handles: %v", out)
	}
	_, out = do(t, h, "GET", "/session/"+sid+"/screenshot", nil)
	if s, _ := out["value"].(string); s == "" {
		t.Fatalf("screenshot: %v", out)
	}
	_, out = do(t, h, "DELETE", "/session/"+sid+"/window", nil)
	if hs, ok := out["value"].([]any); !ok || len(hs) != 0 {
		t.Fatalf("close window: %v", out)
	}
	_, out = do(t, h, "GET", "/session/"+sid+"/title", nil)
	if errCode(out) != ErrInvalidSessionID {
		t.Fatalf("after close window: %v", out)
	}
}

func TestPolicyBlocksNavigation(t *testing.T) {
	h, _, c := newHandler(t)
	c.Policy = core.DomainAllowlist([]string{"example.com"})
	sid := newSession(t, h, nil)
	_, out := do(t, h, "POST", "/session/"+sid+"/url", map[string]any{"url": "https://evil.test/"})
	if errCode(out) != ErrUnsupportedOperation {
		t.Fatalf("policy: %v", out)
	}
	_, out = do(t, h, "POST", "/session/"+sid+"/url", map[string]any{})
	if errCode(out) != ErrInvalidArgument {
		t.Fatalf("missing url: %v", out)
	}
}

func TestTabClosedByUser(t *testing.T) {
	h, b, _ := newHandler(t)
	sid := newSession(t, h, nil)
	b.Destroy(sid)
	_, out := do(t, h, "GET", "/session/"+sid+"/title", nil)
	if errCode(out) != ErrInvalidSessionID {
		t.Fatalf("got %v", out)
	}
}

func TestElementRefs(t *testing.T) {
	if r := elementRef(42); r != "e42" {
		t.Fatal(r)
	}
	for in, ok := range map[string]bool{"e42": true, "e0": false, "x1": false, "e": false, "e-3": false} {
		if _, got := parseElementRef(in); got != ok {
			t.Errorf("parseElementRef(%q) ok=%v", in, got)
		}
	}
	v := replacePlaceholders(map[string]any{
		"a": []any{map[string]any{"__tetherEl": float64(1)}, "x"},
		"b": map[string]any{"__tetherEl": float64(0)},
		"c": map[string]any{"__tetherEl": float64(0), "s": float64(1)},
	}, []int{1, 2})
	b, _ := json.Marshal(v)
	want := `{"a":[{"` + ElementKey + `":"e2"},"x"],"b":{"` + ElementKey + `":"e1"},"c":{"` + ShadowKey + `":"s1"}}`
	if string(b) != want {
		t.Fatalf("got %s", b)
	}
}

func TestEncodeArgs(t *testing.T) {
	_, b, _ := newHandler(t)
	tab, _ := b.NewTab(context.Background())
	args := []json.RawMessage{json.RawMessage(`1`), elementArg("e7"), json.RawMessage(`{"nested":[{"` + ElementKey + `":"e8"}]}`)}
	v, handles, err := encodeArgs(context.Background(), tab, args)
	if err != nil {
		t.Fatal(err)
	}
	if len(handles) != 2 {
		t.Fatalf("handles = %v", handles)
	}
	out, _ := json.Marshal(v)
	if !strings.Contains(string(out), `{"__tetherEl":0}`) || !strings.Contains(string(out), `{"__tetherEl":1}`) {
		t.Fatalf("encoded = %s", out)
	}
	if _, _, err := encodeArgs(context.Background(), tab, []json.RawMessage{elementArg("bogus")}); toError(err).Code != ErrNoSuchElement {
		t.Fatalf("bogus ref: %v", err)
	}
}

func TestWrappersEmbedScriptSafely(t *testing.T) {
	src := "return 1 // trailing comment"
	for _, w := range []string{syncWrapper(src), asyncWrapper(src)} {
		if !strings.Contains(w, src+"\n") {
			t.Fatal("script must be followed by a newline so line comments cannot swallow the wrapper")
		}
	}
}
