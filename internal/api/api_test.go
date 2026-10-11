package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/adeelahmad/tether/internal/cdp/cdptest"
	"github.com/adeelahmad/tether/internal/core"
	"github.com/adeelahmad/tether/internal/policy"
	"github.com/adeelahmad/tether/internal/session"
)

func newAPI(t *testing.T) (*API, *cdptest.Browser) {
	t.Helper()
	b := cdptest.NewBrowser()
	c := core.New(b, session.NewManager(b, session.Options{}), core.Options{})
	return New(c, []byte("PEM"), []string{"proxy"}), b
}

func call(t *testing.T, h http.Handler, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func TestTabLifecycle(t *testing.T) {
	a, b := newAPI(t)
	w, out := call(t, a, "POST", "/tether/v1/tabs", `{"url":"https://example.com/"}`)
	if w.Code != 201 {
		t.Fatalf("create: %d %v", w.Code, out)
	}
	id := out["id"].(string)
	if w.Header().Get("X-Tether-Tab-ID") != id || out["url"] != "https://example.com/" {
		t.Fatalf("create: %v %v", w.Header(), out)
	}
	_, out = call(t, a, "GET", "/tether/v1/tabs", "")
	if tabs := out["tabs"].([]any); len(tabs) != 1 {
		t.Fatalf("list: %v", out)
	}
	if w, _ := call(t, a, "GET", "/tether/v1/tabs/"+id, ""); w.Code != 200 {
		t.Fatalf("get: %d", w.Code)
	}

	tab, _ := b.AttachTab(context.Background(), id)
	tab.(*cdptest.Tab).EvalResult = json.RawMessage(`"Inbox (3)"`)
	w, out = call(t, a, "POST", "/tether/v1/tabs/"+id+"/eval", `{"expression":"document.title"}`)
	if w.Code != 200 || out["value"] != "Inbox (3)" {
		t.Fatalf("eval: %d %v", w.Code, out)
	}

	if w, _ := call(t, a, "DELETE", "/tether/v1/tabs/"+id, ""); w.Code != 204 {
		t.Fatalf("close: %d", w.Code)
	}
	w, out = call(t, a, "DELETE", "/tether/v1/tabs/"+id, "")
	if w.Code != 410 || out["error"] != core.CodeTabGone {
		t.Fatalf("close twice: %d %v", w.Code, out)
	}
	w, _ = call(t, a, "POST", "/tether/v1/tabs/"+id+"/eval", `{"expression":"1"}`)
	if w.Code != 410 {
		t.Fatalf("eval dead tab: %d", w.Code)
	}
}

func TestValidation(t *testing.T) {
	a, _ := newAPI(t)
	tests := []struct {
		method, path, body string
		status             int
	}{
		{"POST", "/tether/v1/tabs", `{"url":`, 400},
		{"POST", "/tether/v1/tabs", `{"wait":"forever"}`, 400},
		{"POST", "/tether/v1/tabs", `{"url":"not a url"}`, 400},
		{"POST", "/tether/v1/tabs/x/eval", `{}`, 400},
		{"GET", "/tether/v1/nope", ``, 404},
	}
	for _, tt := range tests {
		if w, out := call(t, a, tt.method, tt.path, tt.body); w.Code != tt.status {
			t.Errorf("%s %s %s: %d %v", tt.method, tt.path, tt.body, w.Code, out)
		}
	}
}

func TestHealthAndCA(t *testing.T) {
	a, _ := newAPI(t)
	w, out := call(t, a, "GET", "/tether/v1/health", "")
	if w.Code != 200 || out["ok"] != true || out["browser"] != "FakeChrome/1.0" {
		t.Fatalf("health: %v", out)
	}
	w, _ = call(t, a, "GET", "/tether/v1/ca.pem", "")
	if w.Code != 200 || w.Body.String() != "PEM" {
		t.Fatalf("ca: %d %q", w.Code, w.Body.String())
	}
	noCA := New(a.core, nil, nil)
	if w, _ := call(t, noCA, "GET", "/tether/v1/ca.pem", ""); w.Code != 404 {
		t.Fatalf("ca without mitm: %d", w.Code)
	}
}

func callAs(t *testing.T, h http.Handler, p *policy.Principal, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	r = r.WithContext(policy.WithPrincipal(r.Context(), p))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func TestTabPolicy(t *testing.T) {
	b := cdptest.NewBrowser()
	c := core.New(b, session.NewManager(b, session.Options{}), core.Options{
		Policy: core.NewRulesPolicy(policy.Rules{Deny: []string{"evil.test"}}),
	})
	a := New(c, nil, nil)
	agent := &policy.Principal{Name: "agent", Rules: policy.Rules{Allow: []string{"example.com", "github.com"}}}
	other := &policy.Principal{Name: "other"}

	// Create with session rules; navigating outside them fails and the
	// tab is cleaned up.
	w, out := callAs(t, a, agent, "POST", "/tether/v1/tabs", `{"url":"https://github.com/","policy":{"allow":["example.com"]}}`)
	if w.Code != 403 {
		t.Fatalf("create outside session rules: %d %v", w.Code, out)
	}
	if n := len(c.Sessions.List()); n != 0 {
		t.Fatalf("%d tabs left behind", n)
	}
	if w, _ := callAs(t, a, agent, "POST", "/tether/v1/tabs", `{"policy":{"allow":["http://bad"]}}`); w.Code != 400 {
		t.Fatalf("bad pattern: %d", w.Code)
	}

	w, out = callAs(t, a, agent, "POST", "/tether/v1/tabs", `{"url":"https://example.com/","policy":{"deny":["Admin.Example.com"]}}`)
	if w.Code != 201 || out["owner"] != "agent" {
		t.Fatalf("create: %d %v", w.Code, out)
	}
	id := out["id"].(string)

	w, out = callAs(t, a, agent, "GET", "/tether/v1/tabs/"+id+"/policy", "")
	if w.Code != 200 || out["owner"] != "agent" || out["global"] == nil || out["token"] == nil {
		t.Fatalf("get policy: %d %v", w.Code, out)
	}
	if deny := out["session"].(map[string]any)["deny"].([]any); deny[0] != "admin.example.com" {
		t.Fatalf("session rules not normalized: %v", out)
	}

	w, out = callAs(t, a, agent, "PUT", "/tether/v1/tabs/"+id+"/policy", `{"allow":["www.example.com"]}`)
	if w.Code != 200 || out["session"].(map[string]any)["allow"].([]any)[0] != "www.example.com" {
		t.Fatalf("put policy: %d %v", w.Code, out)
	}

	// Another token cannot see, read or change the tab.
	if w, _ := callAs(t, a, other, "GET", "/tether/v1/tabs/"+id+"/policy", ""); w.Code != 410 {
		t.Fatalf("other get policy: %d", w.Code)
	}
	if w, _ := callAs(t, a, other, "PUT", "/tether/v1/tabs/"+id+"/policy", `{}`); w.Code != 410 {
		t.Fatalf("other put policy: %d", w.Code)
	}
	if w, _ := callAs(t, a, other, "DELETE", "/tether/v1/tabs/"+id, ""); w.Code != 410 {
		t.Fatalf("other close: %d", w.Code)
	}
	if _, out := callAs(t, a, other, "GET", "/tether/v1/tabs", ""); len(out["tabs"].([]any)) != 0 {
		t.Fatalf("other sees tabs: %v", out)
	}
	if _, out := callAs(t, a, agent, "GET", "/tether/v1/tabs", ""); len(out["tabs"].([]any)) != 1 {
		t.Fatalf("owner list: %v", out)
	}
	if w, _ := callAs(t, a, agent, "PUT", "/tether/v1/tabs/"+id+"/policy", `{"deny":["a/b"]}`); w.Code != 400 {
		t.Fatalf("bad put: %d", w.Code)
	}
}

func TestEvalForbiddenWithoutScripts(t *testing.T) {
	a, _ := newAPI(t)
	agent := &policy.Principal{Name: "agent"}
	_, out := callAs(t, a, agent, "POST", "/tether/v1/tabs", "")
	id := out["id"].(string)
	if w, out := callAs(t, a, agent, "POST", "/tether/v1/tabs/"+id+"/eval", `{"expression":"1"}`); w.Code != 403 {
		t.Fatalf("eval: %d %v", w.Code, out)
	}
}

func TestWhoami(t *testing.T) {
	a, _ := newAPI(t)
	_, out := call(t, a, "GET", "/tether/v1/whoami", "")
	if out["authenticated"] != false || out["admin"] != true {
		t.Fatalf("anonymous: %v", out)
	}
	p := &policy.Principal{Name: "agent", Rules: policy.Rules{Allow: []string{"example.com"}}}
	_, out = callAs(t, a, p, "GET", "/tether/v1/whoami", "")
	if out["name"] != "agent" || out["admin"] != false || out["scripts"] != false || out["rules"] == nil {
		t.Fatalf("agent: %v", out)
	}
}

func TestLogs(t *testing.T) {
	a, b := newAPI(t)
	_, out := call(t, a, "POST", "/tether/v1/tabs", ``)
	id := out["id"].(string)
	tab, _ := b.AttachTab(context.Background(), id)
	ft := tab.(*cdptest.Tab)

	w, out := call(t, a, "GET", "/tether/v1/tabs/"+id+"/logs", "")
	if w.Code != 200 || len(out["entries"].([]any)) != 0 || out["next"] != float64(0) {
		t.Fatalf("empty: %d %v", w.Code, out)
	}

	ft.Log("info", "one")
	ft.Log("error", "two")
	ft.Log("debug", "three")
	tests := []struct {
		query    string
		code     int
		texts    []string
		wantNext float64
	}{
		{"", 200, []string{"one", "two", "three"}, 3},
		{"?after=1", 200, []string{"two", "three"}, 3},
		{"?level=error", 200, []string{"two"}, 3},
		{"?level=WARN", 200, []string{"two"}, 3},
		{"?after=3", 200, nil, 3},
		{"?after=-1", 400, nil, 0},
		{"?level=loud", 400, nil, 0},
	}
	for _, tt := range tests {
		w, out := call(t, a, "GET", "/tether/v1/tabs/"+id+"/logs"+tt.query, "")
		if w.Code != tt.code {
			t.Fatalf("%s: %d %v", tt.query, w.Code, out)
		}
		if tt.code != 200 {
			continue
		}
		var texts []string
		for _, e := range out["entries"].([]any) {
			texts = append(texts, e.(map[string]any)["text"].(string))
		}
		if !reflect.DeepEqual(texts, tt.texts) || out["next"] != tt.wantNext {
			t.Fatalf("%s: got %v next=%v", tt.query, texts, out["next"])
		}
	}

	if w, _ := call(t, a, "GET", "/tether/v1/tabs/NOPE/logs", ""); w.Code != http.StatusGone {
		t.Fatalf("unknown tab: %d", w.Code)
	}
}
