package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/adeelahmad/tether/internal/cdp/cdptest"
	"github.com/adeelahmad/tether/internal/core"
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
