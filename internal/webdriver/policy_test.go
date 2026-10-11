package webdriver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/adeelahmad/tether/internal/cdp/cdptest"
	"github.com/adeelahmad/tether/internal/policy"
)

func doAs(t *testing.T, h http.Handler, p *policy.Principal, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, path, &buf)
	r = r.WithContext(policy.WithPrincipal(r.Context(), p))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestSessionPolicyCapability(t *testing.T) {
	h, b, _ := newHandler(t)
	agent := &policy.Principal{Name: "agent"}
	other := &policy.Principal{Name: "other"}

	code, out := doAs(t, h, agent, "POST", "/session", map[string]any{"capabilities": map[string]any{"alwaysMatch": map[string]any{
		"tether:options": map[string]any{"policy": map[string]any{"allow": []string{"Example.com"}}},
	}}})
	if code != 200 {
		t.Fatalf("new session: %d %v", code, out)
	}
	v := out["value"].(map[string]any)
	sid := v["sessionId"].(string)
	opts := v["capabilities"].(map[string]any)["tether:options"].(map[string]any)
	if opts["policy"].(map[string]any)["allow"].([]any)[0] != "example.com" {
		t.Fatalf("policy not echoed: %v", opts)
	}
	tab, _ := b.AttachTab(context.Background(), sid)
	if !tab.(*cdptest.Tab).Guarded() {
		t.Fatal("restricted session tab is not guarded")
	}

	if code, out := doAs(t, h, agent, "POST", "/session/"+sid+"/url", map[string]any{"url": "https://evil.test/"}); code == 200 {
		t.Fatalf("navigate outside session policy: %d %v", code, out)
	}
	if code, out := doAs(t, h, agent, "POST", "/session/"+sid+"/url", map[string]any{"url": "https://www.example.com/"}); code != 200 {
		t.Fatalf("navigate: %d %v", code, out)
	}

	// Another token cannot drive or delete the session.
	if code, out := doAs(t, h, other, "GET", "/session/"+sid+"/url", nil); code != 404 || errCode(out) != ErrInvalidSessionID {
		t.Fatalf("other token: %d %v", code, out)
	}
	if code, _ := doAs(t, h, other, "DELETE", "/session/"+sid, nil); code != 404 {
		t.Fatalf("other delete: %d", code)
	}

	// Scripts need the scripts permission.
	if code, out := doAs(t, h, agent, "POST", "/session/"+sid+"/execute/sync", map[string]any{"script": "return 1", "args": []any{}}); code == 200 {
		t.Fatalf("execute without scripts permission: %d %v", code, out)
	}

	if code, _ := doAs(t, h, agent, "DELETE", "/session/"+sid, nil); code != 200 {
		t.Fatalf("owner delete: %d", code)
	}

	code, out = doAs(t, h, agent, "POST", "/session", map[string]any{"capabilities": map[string]any{"alwaysMatch": map[string]any{
		"tether:options": map[string]any{"policy": map[string]any{"deny": []string{"a/b"}}},
	}}})
	if code != 400 || errCode(out) != ErrInvalidArgument {
		t.Fatalf("bad policy: %d %v", code, out)
	}
}

// Popups and new windows belong to the session: same owner, same session
// rules, so a restricted session cannot leave its allowlist through them.
func TestPopupsAndNewWindowsInheritPolicy(t *testing.T) {
	h, b, _ := newHandler(t)
	agent := &policy.Principal{Name: "agent"}
	_, out := doAs(t, h, agent, "POST", "/session", map[string]any{"capabilities": map[string]any{"alwaysMatch": map[string]any{
		"tether:options": map[string]any{"policy": map[string]any{"allow": []string{"example.com"}}},
	}}})
	sid := out["value"].(map[string]any)["sessionId"].(string)

	b.AddPopup("GOOD", "https://www.example.com/p", sid)
	b.AddPopup("BAD", "https://evil.test/", sid)
	if code, out := doAs(t, h, agent, "POST", "/session/"+sid+"/window", map[string]any{"handle": "GOOD"}); code != 200 {
		t.Fatalf("switch to allowed popup (a non-admin token may take its own popups): %d %v", code, out)
	}
	good, _ := b.AttachTab(context.Background(), "GOOD")
	if !good.(*cdptest.Tab).Guarded() {
		t.Fatal("popup is not guarded by the session rules")
	}
	if code, out := doAs(t, h, agent, "POST", "/session/"+sid+"/url", map[string]any{"url": "https://evil.test/"}); code == 200 {
		t.Fatalf("popup navigated outside session rules: %v", out)
	}

	doAs(t, h, agent, "POST", "/session/"+sid+"/window", map[string]any{"handle": sid})
	if code, out := doAs(t, h, agent, "POST", "/session/"+sid+"/window", map[string]any{"handle": "BAD"}); code == 200 {
		t.Fatalf("switched to a popup on a forbidden site: %v", out)
	}
	for _, id := range b.Open() {
		if id == "BAD" {
			t.Fatal("forbidden popup left open")
		}
	}

	_, out = doAs(t, h, agent, "POST", "/session/"+sid+"/window/new", map[string]any{})
	nw := out["value"].(map[string]any)["handle"].(string)
	doAs(t, h, agent, "POST", "/session/"+sid+"/window", map[string]any{"handle": nw})
	if code, out := doAs(t, h, agent, "POST", "/session/"+sid+"/url", map[string]any{"url": "https://evil.test/"}); code == 200 {
		t.Fatalf("new window navigated outside session rules: %v", out)
	}
	if code, out := doAs(t, h, agent, "POST", "/session/"+sid+"/url", map[string]any{"url": "https://example.com/"}); code != 200 {
		t.Fatalf("new window: %d %v", code, out)
	}
}
