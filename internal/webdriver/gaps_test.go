package webdriver

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/adeelahmad/tether/internal/cdp"
	"github.com/adeelahmad/tether/internal/cdp/cdptest"
)

func value(out map[string]any) any { return out["value"] }

func fakeTab(t *testing.T, b *cdptest.Browser, id string) *cdptest.Tab {
	t.Helper()
	tab, err := b.AttachTab(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return tab.(*cdptest.Tab)
}

func TestSwitchFrame(t *testing.T) {
	h, b, _ := newHandler(t)
	sid := newSession(t, h, nil)
	tab := fakeTab(t, b, sid)
	tab.CallResult = json.RawMessage(`{"v": "true", "e": null}`) // scriptIsFrame → true
	frame := func(id any) (int, map[string]any) {
		return do(t, h, "POST", "/session/"+sid+"/frame", map[string]any{"id": id})
	}
	if code, out := frame(map[string]any{ElementKey: "e5"}); code != 200 {
		t.Fatalf("switch: %d %v", code, out)
	}
	do(t, h, "GET", "/session/"+sid+"/source", nil)
	if len(tab.Frame) != 1 || tab.Frame[0] != 5 {
		t.Fatalf("frame path = %v", tab.Frame)
	}
	frame(map[string]any{ElementKey: "e9"})
	do(t, h, "GET", "/session/"+sid+"/source", nil)
	if len(tab.Frame) != 2 || tab.Frame[1] != 9 {
		t.Fatalf("nested path = %v", tab.Frame)
	}
	tab.Frame = nil
	do(t, h, "POST", "/session/"+sid+"/frame/parent", map[string]any{})
	do(t, h, "GET", "/session/"+sid+"/source", nil)
	if len(tab.Frame) != 1 || tab.Frame[0] != 5 {
		t.Fatalf("after parent = %v", tab.Frame)
	}
	tab.Frame = nil
	frame(nil)
	do(t, h, "GET", "/session/"+sid+"/source", nil)
	if tab.Frame != nil {
		t.Fatalf("top should not use a frame view: %v", tab.Frame)
	}

	tab.CallResult = json.RawMessage(`{"v": "false", "e": null}`)
	if _, out := frame(map[string]any{ElementKey: "e5"}); errCode(out) != ErrNoSuchFrame {
		t.Fatalf("non-frame element: %v", out)
	}
	tab.CallResult = json.RawMessage(`{"v": "null", "e": null}`)
	if _, out := frame(3); errCode(out) != ErrNoSuchFrame {
		t.Fatalf("missing index: %v", out)
	}
	for _, bad := range []any{-1, 1.5, "x", math.MaxUint16 + 1, map[string]any{"nope": 1}} {
		if _, out := frame(bad); errCode(out) != ErrInvalidArgument {
			t.Fatalf("id %v: %v", bad, out)
		}
	}

	// Navigation returns to the top-level context.
	tab.CallResult = json.RawMessage(`{"v": "true", "e": null}`)
	frame(map[string]any{ElementKey: "e5"})
	do(t, h, "POST", "/session/"+sid+"/url", map[string]any{"url": "https://example.com/"})
	tab.Frame = nil
	do(t, h, "GET", "/session/"+sid+"/source", nil)
	if tab.Frame != nil {
		t.Fatalf("navigation should reset frames: %v", tab.Frame)
	}
}

func TestShadowRootCommands(t *testing.T) {
	h, b, _ := newHandler(t)
	sid := newSession(t, h, nil)
	tab := fakeTab(t, b, sid)
	_, out := do(t, h, "GET", "/session/"+sid+"/element/e3/shadow", nil)
	if errCode(out) != ErrNoSuchShadowRoot {
		t.Fatalf("no shadow: %v", out)
	}
	tab.Shadow = 77
	_, out = do(t, h, "GET", "/session/"+sid+"/element/e3/shadow", nil)
	if value(out).(map[string]any)[ShadowKey] != "s77" {
		t.Fatalf("shadow: %v", out)
	}
	tab.CallResult = json.RawMessage(`{"v": "[]", "e": null}`)
	_, out = do(t, h, "POST", "/session/"+sid+"/shadow/s77/elements", map[string]any{"using": "css selector", "value": "span"})
	if v, ok := value(out).([]any); !ok || len(v) != 0 {
		t.Fatalf("find from shadow: %v", out)
	}
	_, out = do(t, h, "POST", "/session/"+sid+"/shadow/s77/element", map[string]any{"using": "css selector", "value": "span"})
	if errCode(out) != ErrNoSuchElement {
		t.Fatalf("find one from shadow: %v", out)
	}
	_, out = do(t, h, "POST", "/session/"+sid+"/shadow/s77/element", map[string]any{"using": "xpath", "value": "//span"})
	if errCode(out) != ErrInvalidArgument {
		t.Fatalf("xpath from shadow: %v", out)
	}
	_, out = do(t, h, "POST", "/session/"+sid+"/shadow/e77/element", map[string]any{"using": "css selector", "value": "span"})
	if errCode(out) != ErrNoSuchShadowRoot {
		t.Fatalf("element ref as shadow: %v", out)
	}
	last := tab.Calls[len(tab.Calls)-1]
	args, _ := json.Marshal(last.Args[0].Value)
	if !bytes.Contains(args, []byte(`"s":1`)) {
		t.Fatalf("shadow root should be passed as a shadow placeholder: %s", args)
	}
}

func TestAlerts(t *testing.T) {
	h, b, _ := newHandler(t)
	sid := newSession(t, h, nil)
	tab := fakeTab(t, b, sid)
	for _, p := range []string{"/alert/text", "/alert/accept", "/alert/dismiss"} {
		method := "POST"
		if p == "/alert/text" {
			method = "GET"
		}
		if _, out := do(t, h, method, "/session/"+sid+p, map[string]any{}); errCode(out) != ErrNoSuchAlert {
			t.Fatalf("%s without alert: %v", p, out)
		}
	}

	tab.OpenDialog(cdp.Dialog{Type: "prompt", Message: "name?"})
	if _, out := do(t, h, "GET", "/session/"+sid+"/alert/text", nil); value(out) != "name?" {
		t.Fatalf("text: %v", out)
	}
	do(t, h, "POST", "/session/"+sid+"/alert/text", map[string]any{"text": "ada"})
	do(t, h, "POST", "/session/"+sid+"/alert/accept", map[string]any{})
	if len(tab.Answers) != 1 || !tab.Answers[0].Accept || tab.Answers[0].Text == nil || *tab.Answers[0].Text != "ada" {
		t.Fatalf("answers = %+v", tab.Answers)
	}

	tab.OpenDialog(cdp.Dialog{Type: "alert", Message: "hi"})
	if _, out := do(t, h, "POST", "/session/"+sid+"/alert/text", map[string]any{"text": "x"}); errCode(out) != ErrElementNotInteractable {
		t.Fatalf("text into alert: %v", out)
	}
	do(t, h, "POST", "/session/"+sid+"/alert/dismiss", map[string]any{})
	if a := tab.Answers[len(tab.Answers)-1]; a.Accept || a.Text != nil {
		t.Fatalf("dismiss = %+v", a)
	}

	// dismiss and notify (the default).
	tab.OpenDialog(cdp.Dialog{Type: "confirm", Message: "sure?"})
	code, out := do(t, h, "GET", "/session/"+sid+"/title", nil)
	if code != 500 || errCode(out) != ErrUnexpectedAlertOpen || value(out).(map[string]any)["data"].(map[string]any)["text"] != "sure?" {
		t.Fatalf("unhandled prompt: %d %v", code, out)
	}
	if tab.Dialog() != nil || tab.Answers[len(tab.Answers)-1].Accept {
		t.Fatal("prompt should have been dismissed")
	}
	// Session-level commands ignore prompts.
	tab.OpenDialog(cdp.Dialog{Type: "alert", Message: "x"})
	if code, _ := do(t, h, "GET", "/session/"+sid+"/timeouts", nil); code != 200 {
		t.Fatal("timeouts should not touch prompts")
	}
}

func TestPromptBehaviors(t *testing.T) {
	tests := []struct {
		behavior string
		accepted bool
		handled  bool
		notify   bool
	}{
		{"accept", true, true, false},
		{"dismiss", false, true, false},
		{"accept and notify", true, true, true},
		{"dismiss and notify", false, true, true},
		{"ignore", false, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.behavior, func(t *testing.T) {
			h, b, _ := newHandler(t)
			sid := newSession(t, h, map[string]any{"unhandledPromptBehavior": tt.behavior})
			tab := fakeTab(t, b, sid)
			tab.OpenDialog(cdp.Dialog{Type: "confirm", Message: "?"})
			_, out := do(t, h, "GET", "/session/"+sid+"/title", nil)
			if notified := errCode(out) == ErrUnexpectedAlertOpen; notified != tt.notify {
				t.Fatalf("notify = %v: %v", notified, out)
			}
			if handled := len(tab.Answers) == 1; handled != tt.handled {
				t.Fatalf("handled = %v", handled)
			}
			if tt.handled && tab.Answers[0].Accept != tt.accepted {
				t.Fatalf("accepted = %v", tab.Answers[0].Accept)
			}
		})
	}
	h, _, _ := newHandler(t)
	_, out := do(t, h, "POST", "/session", map[string]any{"capabilities": map[string]any{"alwaysMatch": map[string]any{"unhandledPromptBehavior": "shrug"}}})
	if errCode(out) != ErrInvalidArgument {
		t.Fatalf("bad behaviour: %v", out)
	}
	sid := newSession(t, h, map[string]any{"unhandledPromptBehavior": map[string]any{"default": "accept"}})
	if sid == "" {
		t.Fatal("map form should be accepted")
	}
}

func TestWindowsAndPopups(t *testing.T) {
	h, b, _ := newHandler(t)
	b.AddTab("OLD", "https://old.example/")
	b.AddPopup("OLDPOP", "https://old.example/p", "OLD")
	sid := newSession(t, h, nil)
	b.AddPopup("POP", "https://example.com/p", sid)
	b.AddPopup("POP2", "https://example.com/p2", "POP")
	b.AddPopup("OTHER", "https://example.com/o", "OLD")

	_, out := do(t, h, "GET", "/session/"+sid+"/window/handles", nil)
	hs, _ := json.Marshal(value(out))
	if string(hs) != `["`+sid+`","POP","POP2"]` {
		t.Fatalf("handles = %s", hs)
	}
	if code, out := do(t, h, "POST", "/session/"+sid+"/window", map[string]any{"handle": "POP2"}); code != 200 {
		t.Fatalf("switch to popup: %v", out)
	}
	if _, out := do(t, h, "GET", "/session/"+sid+"/window", nil); value(out) != "POP2" {
		t.Fatalf("current = %v", out)
	}
	if _, out := do(t, h, "POST", "/session/"+sid+"/window", map[string]any{"handle": "OTHER"}); errCode(out) != ErrNoSuchWindow {
		t.Fatalf("someone else's tab: %v", out)
	}
	if _, out := do(t, h, "POST", "/session/"+sid+"/window", map[string]any{}); errCode(out) != ErrInvalidArgument {
		t.Fatalf("missing handle: %v", out)
	}

	_, out = do(t, h, "DELETE", "/session/"+sid+"/window", nil)
	if left, _ := json.Marshal(value(out)); string(left) != `["`+sid+`","POP"]` {
		t.Fatalf("after closing POP2 = %s", left)
	}
	if _, out := do(t, h, "GET", "/session/"+sid+"/title", nil); errCode(out) != ErrNoSuchWindow {
		t.Fatalf("closed current window: %v", out)
	}
	if code, _ := do(t, h, "GET", "/session/"+sid+"/window/handles", nil); code != 200 {
		t.Fatal("handles must work without a current window")
	}

	do(t, h, "POST", "/session/"+sid+"/window", map[string]any{"handle": sid})
	_, out = do(t, h, "POST", "/session/"+sid+"/window/new", map[string]any{"type": "window"})
	nw := value(out).(map[string]any)
	if nw["type"] != "window" {
		t.Fatalf("new window = %v", nw)
	}
	_, out = do(t, h, "POST", "/session/"+sid+"/window/new", map[string]any{})
	if value(out).(map[string]any)["type"] != "tab" {
		t.Fatalf("new tab = %v", out)
	}

	do(t, h, "DELETE", "/session/"+sid, nil)
	open := map[string]bool{}
	for _, id := range b.Open() {
		open[id] = true
	}
	if len(open) != 3 || !open["OLD"] || !open["OLDPOP"] || !open["OTHER"] {
		t.Fatalf("delete should close exactly the session's windows; open = %v", open)
	}
}

func TestLastWindowEndsSession(t *testing.T) {
	h, b, _ := newHandler(t)
	sid := newSession(t, h, nil)
	b.AddPopup("POP", "https://example.com/p", sid)
	do(t, h, "GET", "/session/"+sid+"/window/handles", nil)
	b.Destroy(sid)
	if _, out := do(t, h, "GET", "/session/"+sid+"/title", nil); errCode(out) != ErrNoSuchWindow {
		t.Fatalf("session should outlive its first window: %v", out)
	}
	do(t, h, "POST", "/session/"+sid+"/window", map[string]any{"handle": "POP"})
	if code, out := do(t, h, "GET", "/session/"+sid+"/title", nil); code != 200 {
		t.Fatalf("popup: %v", out)
	}
	do(t, h, "DELETE", "/session/"+sid+"/window", nil)
	if _, out := do(t, h, "GET", "/session/"+sid+"/window/handles", nil); errCode(out) != ErrInvalidSessionID {
		t.Fatalf("closing the last window should end the session: %v", out)
	}
}

func TestWindowRect(t *testing.T) {
	h, b, _ := newHandler(t)
	b.AddTab("USER", "https://mail.example.com/")
	shared := newSession(t, h, nil)
	_, out := do(t, h, "POST", "/session/"+shared+"/window/rect", map[string]any{"width": 500, "height": 400})
	if value(out).(map[string]any)["width"] != float64(1280) {
		t.Fatalf("a window shared with the person's tabs must not change: %v", out)
	}
	do(t, h, "POST", "/session/"+shared+"/window/maximize", map[string]any{})
	if _, ok := b.Bounds[1]; ok {
		t.Fatal("shared window was changed")
	}

	own := newSession(t, h, map[string]any{"tether:options": map[string]any{"newWindow": true}})
	_, out = do(t, h, "POST", "/session/"+own+"/window/rect", map[string]any{"x": 5, "y": 6, "width": 500, "height": 400})
	if r := value(out).(map[string]any); r["width"] != float64(500) || r["x"] != float64(5) {
		t.Fatalf("own window rect = %v", r)
	}
	do(t, h, "POST", "/session/"+own+"/window/fullscreen", map[string]any{})
	w, _ := b.Window(context.Background(), own)
	if w.State != "fullscreen" {
		t.Fatalf("state = %v", w.State)
	}
	_, out = do(t, h, "POST", "/session/"+own+"/window/rect", map[string]any{"width": nil, "height": 300})
	if r := value(out).(map[string]any); r["width"] != float64(500) || r["height"] != float64(300) {
		t.Fatalf("partial rect = %v", r)
	}
	for _, bad := range []map[string]any{{"width": -1}, {"x": 1.5}, {"height": "tall"}} {
		if _, out := do(t, h, "POST", "/session/"+own+"/window/rect", bad); errCode(out) != ErrInvalidArgument {
			t.Fatalf("%v: %v", bad, out)
		}
	}
}

func TestPDFOptions(t *testing.T) {
	o, err := pdfOptions(nil)
	if err != nil || o.Landscape || o.Scale != 1 || math.Abs(o.PaperWidth-8.5) > 0.001 || math.Abs(o.PaperHeight-11) > 0.001 || math.Abs(o.MarginTop-1/2.54) > 0.001 {
		t.Fatalf("defaults = %+v %v", o, err)
	}
	o, err = pdfOptions([]byte(`{"orientation":"landscape","scale":0.5,"background":true,"page":{"width":2.54},"margin":{"left":0},"pageRanges":[1,"3-5","7-"," -2 "]}`))
	if err != nil || !o.Landscape || o.Scale != 0.5 || !o.PrintBackground || o.PaperWidth != 1 || o.MarginLeft != 0 || o.PageRanges != "1,3-5,7-,-2" {
		t.Fatalf("custom = %+v %v", o, err)
	}
	for _, bad := range []string{
		`{"orientation":"sideways"}`, `{"scale":3}`, `{"scale":0}`, `{"page":{"width":0}}`,
		`{"margin":{"top":-1}}`, `{"pageRanges":[0]}`, `{"pageRanges":["a-b"]}`, `{"pageRanges":["-"]}`, `{"pageRanges":[true]}`,
	} {
		if _, err := pdfOptions([]byte(bad)); err == nil || toError(err).Code != ErrInvalidArgument {
			t.Errorf("%s: %v", bad, err)
		}
	}

	h, b, _ := newHandler(t)
	sid := newSession(t, h, nil)
	_, out := do(t, h, "POST", "/session/"+sid+"/print", map[string]any{"orientation": "landscape"})
	raw, _ := base64.StdEncoding.DecodeString(value(out).(string))
	if !bytes.HasPrefix(raw, []byte("%PDF")) || !fakeTab(t, b, sid).PDFs[0].Landscape {
		t.Fatalf("print: %v", out)
	}
}

func zipOf(t *testing.T, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		f, _ := zw.Create(name)
		f.Write([]byte(body))
	}
	zw.Close()
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestUploadAndFileInput(t *testing.T) {
	h, b, _ := newHandler(t)
	sid := newSession(t, h, nil)
	tab := fakeTab(t, b, sid)

	_, out := do(t, h, "POST", "/session/"+sid+"/se/file", map[string]any{"file": zipOf(t, map[string]string{"../../evil/report.csv": "a,b"})})
	path, _ := value(out).(string)
	if filepath.Base(path) != "report.csv" {
		t.Fatalf("upload: %v", out)
	}
	if body, err := os.ReadFile(path); err != nil || string(body) != "a,b" {
		t.Fatalf("uploaded file: %q %v", body, err)
	}
	for _, bad := range []string{"!!!", base64.StdEncoding.EncodeToString([]byte("not a zip")), zipOf(t, map[string]string{"a": "1", "b": "2"}), zipOf(t, nil)} {
		if _, out := do(t, h, "POST", "/session/"+sid+"/se/file", map[string]any{"file": bad}); errCode(out) != ErrInvalidArgument {
			t.Fatalf("bad upload accepted: %v", out)
		}
	}

	tab.CallResult = json.RawMessage(`{"v": "\"file\"", "e": null}`)
	if code, out := do(t, h, "POST", "/session/"+sid+"/element/e4/value", map[string]any{"text": path}); code != 200 {
		t.Fatalf("set file: %v", out)
	}
	if got := tab.Files[4]; len(got) != 1 || got[0] != path {
		t.Fatalf("files = %v", got)
	}
	other := filepath.Join(t.TempDir(), "b.txt")
	os.WriteFile(other, []byte("b"), 0o600)
	if _, out := do(t, h, "POST", "/session/"+sid+"/element/e4/value", map[string]any{"text": path + "\n" + other}); errCode(out) != ErrInvalidArgument {
		t.Fatalf("two files into a single input: %v", out)
	}
	if _, out := do(t, h, "POST", "/session/"+sid+"/element/e4/value", map[string]any{"text": "/no/such/file"}); errCode(out) != ErrInvalidArgument {
		t.Fatalf("missing file: %v", out)
	}
	tab.CallResult = json.RawMessage(`{"v": "\"files\"", "e": null}`)
	do(t, h, "POST", "/session/"+sid+"/element/e4/value", map[string]any{"text": path + "\n" + other + "\n"})
	if got := tab.Files[4]; len(got) != 2 {
		t.Fatalf("multiple files = %v", got)
	}

	do(t, h, "DELETE", "/session/"+sid, nil)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("upload should be removed with the session: %v", err)
	}
}
