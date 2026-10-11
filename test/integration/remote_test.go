//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestRemoteUI drives a real tab the way a person in the web UI does:
// live frames over the websocket, a real click into a form field, typed
// keys, a paste, and navigation from the address bar.
func TestRemoteUI(t *testing.T) {
	s := site()
	defer s.Close()
	base, _ := startTether(t)
	direct := &http.Client{Timeout: 30 * time.Second}

	resp, body := send(t, direct, "GET", base+"/remote/", nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(body, `id="screen"`) {
		t.Fatalf("ui page: %d", resp.StatusCode)
	}

	resp, body = send(t, direct, "POST", base+"/remote/api/tabs", strings.NewReader(`{"url":"`+s.URL+`/me"}`), nil)
	if resp.StatusCode != 201 {
		t.Fatalf("open tab: %d %s", resp.StatusCode, body)
	}
	tab := resp.Header.Get("X-Tether-Tab-ID")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/remote/ws/"+tab, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	ws.SetReadLimit(16 << 20)

	// Frames arrive in the background; keep the latest metadata.
	type msg struct {
		T, URL, Message, Code string
	}
	texts := make(chan msg, 64)
	frames := make(chan float64, 64) // deviceWidth of each frame
	go func() {
		for {
			typ, b, err := ws.Read(ctx)
			if err != nil {
				close(texts)
				return
			}
			if typ == websocket.MessageBinary {
				n := binary.BigEndian.Uint32(b)
				var meta struct{ DeviceWidth float64 }
				json.Unmarshal(b[4:4+n], &meta)
				if !bytes.HasPrefix(b[4+n:], []byte{0xff, 0xd8}) {
					t.Errorf("frame is not a JPEG")
				}
				select {
				case frames <- meta.DeviceWidth:
				default:
				}
				continue
			}
			var m msg
			json.Unmarshal(b, &m)
			if m.T == "error" {
				t.Errorf("server error: %s %s", m.Code, m.Message)
			}
			texts <- m
		}
	}()
	select {
	case w := <-frames:
		if w <= 0 {
			t.Fatalf("frame without viewport size")
		}
	case <-ctx.Done():
		t.Fatal("no video frame")
	}

	write := func(m map[string]any) {
		b, _ := json.Marshal(m)
		if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
	}
	eval := func(expr string) string {
		_, body := send(t, direct, "POST", base+"/tether/v1/tabs/"+tab+"/eval",
			strings.NewReader(`{"expression":`+jsonString(expr)+`}`), nil)
		var out struct{ Value any }
		json.Unmarshal([]byte(body), &out)
		b, _ := json.Marshal(out.Value)
		return string(b)
	}
	waitEval := func(expr, want string) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			got := eval(expr)
			if got == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s = %s, want %s", expr, got, want)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	waitEval("document.readyState", `"complete"`)

	// Click the search box at its on-screen position, like a person would.
	var rect struct{ X, Y float64 }
	json.Unmarshal([]byte(eval(`(() => { const r = document.getElementById('q').getBoundingClientRect(); return {x: r.x + r.width/2, y: r.y + r.height/2}; })()`)), &rect)
	for _, typ := range []string{"mouseMoved", "mousePressed", "mouseReleased"} {
		write(map[string]any{"t": "mouse", "type": typ, "x": rect.X, "y": rect.Y, "button": "left", "buttons": 1, "clickCount": 1})
	}
	waitEval("document.activeElement.id", `"q"`)

	for _, k := range []string{"h", "i"} {
		write(map[string]any{"t": "key", "type": "keyDown", "key": k, "code": "Key" + strings.ToUpper(k), "text": k})
		write(map[string]any{"t": "key", "type": "keyUp", "key": k, "code": "Key" + strings.ToUpper(k)})
	}
	write(map[string]any{"t": "text", "text": " there"})
	waitEval("document.getElementById('q').value", `"hi there"`)

	// Enter submits the form: a real navigation, reported back as tab info.
	write(map[string]any{"t": "key", "type": "keyDown", "key": "Enter", "code": "Enter", "keyCode": 13, "text": "\r"})
	write(map[string]any{"t": "key", "type": "keyUp", "key": "Enter", "code": "Enter", "keyCode": 13})
	waitInfo := func(want string) {
		t.Helper()
		for m := range texts {
			if m.T == "info" && strings.Contains(m.URL, want) {
				return
			}
		}
		t.Fatalf("no info for %s", want)
	}
	waitInfo("/search?q=hi+there")

	// The address bar navigates the tab, and back goes back.
	write(map[string]any{"t": "nav", "url": s.URL + "/me?from=ui"})
	waitInfo("/me?from=ui")
	write(map[string]any{"t": "back"})
	waitInfo("/search?q=hi+there")

	// JavaScript dialogs are relayed, answered from the UI, and unblock
	// the page.
	eval(`setTimeout(() => { document.title = 'answer:' + prompt('Name?', 'x'); }, 0)`)
	for m := range texts {
		if m.T == "dialog" {
			break
		}
	}
	write(map[string]any{"t": "dialog", "accept": true, "text": "Ada"})
	waitEval("document.title", `"answer:Ada"`)

	// Closing the tab ends the stream with a clear reason.
	if resp, _ := send(t, direct, "DELETE", base+"/remote/api/tabs/"+tab, nil, nil); resp.StatusCode != 204 {
		t.Fatalf("close: %d", resp.StatusCode)
	}
	for m := range texts {
		if m.T == "ended" {
			return
		}
	}
	t.Fatal("stream did not report the closed tab")
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
