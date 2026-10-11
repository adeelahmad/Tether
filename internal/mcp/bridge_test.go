package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// authed requires a bearer token, like Tether's server does.
func authed(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func runBridge(t *testing.T, url, token, input string) []map[string]any {
	t.Helper()
	var out strings.Builder
	err := Bridge(context.Background(), BridgeOptions{URL: url, Token: token}, strings.NewReader(input), &out)
	if err != nil {
		t.Fatal(err)
	}
	var msgs []map[string]any
	sc := bufio.NewScanner(strings.NewReader(out.String()))
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("stdout line is not one JSON message: %q", sc.Text())
		}
		msgs = append(msgs, m)
	}
	return msgs
}

func TestBridge(t *testing.T) {
	e := newEnv(t, Options{})
	const token = "bridge-token-0123456789"
	var mu sync.Mutex
	var deleted []string
	ts := httptest.NewServer(authed(token, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			mu.Lock()
			deleted = append(deleted, r.Header.Get(HeaderSessionID))
			mu.Unlock()
		}
		if r.Method == http.MethodPost && r.Header.Get(HeaderSessionID) != "" && r.Header.Get(HeaderProtocolVersion) != "2025-03-26" {
			t.Errorf("bridge did not send the negotiated protocol version")
		}
		e.srv.ServeHTTP(w, r)
	})))
	defer ts.Close()

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","clientInfo":{"name":"desktop"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		``,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"browser_navigate","arguments":{"url":"https://example.com/"}}}`,
		`not json`,
	}, "\n")
	msgs := runBridge(t, ts.URL+Prefix, token, input)
	ids := map[float64]map[string]any{}
	for _, m := range msgs {
		if id, ok := m["id"].(float64); ok {
			ids[id] = m
		}
	}
	if len(msgs) != 4 || ids[1] == nil || ids[2] == nil || ids[3] == nil {
		t.Fatalf("responses = %v", msgs)
	}
	if v := ids[1]["result"].(map[string]any)["protocolVersion"]; v != "2025-03-26" {
		t.Fatalf("protocol = %v", v)
	}
	if !strings.Contains(mustMarshal(ids[3]), "Loaded https://example.com/") {
		t.Fatalf("tool call = %v", ids[3])
	}
	if len(deleted) != 1 || deleted[0] == "" {
		t.Fatalf("session not terminated on EOF: %v", deleted)
	}
	if len(e.b.Open()) != 0 {
		t.Fatalf("tabs left open after the bridge exited: %v", e.b.Open())
	}

	// A wrong token surfaces as a JSON-RPC error for the request.
	msgs = runBridge(t, ts.URL+Prefix, "wrong-token-0123456789", input)
	if len(msgs) == 0 || !strings.Contains(mustMarshal(msgs[0]), "rejected the token") {
		t.Fatalf("bad token = %v", msgs)
	}
}

func TestBridgeUnreachableAndSSE(t *testing.T) {
	msgs := runBridge(t, "http://127.0.0.1:1/mcp", "", `{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n"+`{"jsonrpc":"2.0","method":"notifications/x"}`)
	if len(msgs) != 1 || !strings.Contains(mustMarshal(msgs[0]), "cannot reach Tether") {
		t.Fatalf("unreachable = %v", msgs)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set(HeaderSessionID, "s1")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\ndata: \"id\":1,\"result\":{}}\n\n")
	}))
	defer ts.Close()
	msgs = runBridge(t, ts.URL, "", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	if len(msgs) != 1 || msgs[0]["id"] != float64(1) {
		t.Fatalf("sse = %v", msgs)
	}

	if err := Bridge(context.Background(), BridgeOptions{}, strings.NewReader(""), io.Discard); err == nil {
		t.Fatal("expected an error without a URL")
	}
}

func mustMarshal(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
