package webdriver

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/adeelahmad/tether/internal/cdp"
	"github.com/adeelahmad/tether/internal/cdp/cdptest"
)

// actionsSession opens a session whose fake tab reports an 800x600
// viewport. pageLoadStrategy none keeps settle from polling readyState.
func actionsSession(t *testing.T) (http.Handler, string, *cdptest.Tab) {
	t.Helper()
	h, b, _ := newHandler(t)
	sid := newSession(t, h, map[string]any{"pageLoadStrategy": "none"})
	tab, err := b.AttachTab(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	ft := tab.(*cdptest.Tab)
	ft.EvalResult = json.RawMessage(`[800, 600]`)
	return h, sid, ft
}

func perform(t *testing.T, h http.Handler, sid string, seqs ...map[string]any) map[string]any {
	t.Helper()
	code, out := do(t, h, "POST", "/session/"+sid+"/actions", map[string]any{"actions": seqs})
	if code != 200 {
		t.Fatalf("perform: %d %v", code, out)
	}
	return out
}

func mouse(id string, acts ...map[string]any) map[string]any {
	return map[string]any{"type": "pointer", "id": id, "parameters": map[string]any{"pointerType": "mouse"}, "actions": acts}
}

func touch(id string, acts ...map[string]any) map[string]any {
	return map[string]any{"type": "pointer", "id": id, "parameters": map[string]any{"pointerType": "touch"}, "actions": acts}
}

func keys(id string, acts ...map[string]any) map[string]any {
	return map[string]any{"type": "key", "id": id, "actions": acts}
}

func move(x, y float64) map[string]any {
	return map[string]any{"type": "pointerMove", "x": x, "y": y}
}
func down(b int) map[string]any       { return map[string]any{"type": "pointerDown", "button": b} }
func up(b int) map[string]any         { return map[string]any{"type": "pointerUp", "button": b} }
func keyDown(v string) map[string]any { return map[string]any{"type": "keyDown", "value": v} }
func keyUp(v string) map[string]any   { return map[string]any{"type": "keyUp", "value": v} }
func pause(ms int) map[string]any     { return map[string]any{"type": "pause", "duration": ms} }

func mouseEvents(evs []any) []cdp.MouseEvent {
	var out []cdp.MouseEvent
	for _, e := range evs {
		if m, ok := e.(cdp.MouseEvent); ok {
			out = append(out, m)
		}
	}
	return out
}

func TestActionsValidation(t *testing.T) {
	tests := []struct {
		name string
		body any
	}{
		{"no actions", map[string]any{}},
		{"actions not array", map[string]any{"actions": "nope"}},
		{"bad source type", map[string]any{"actions": []any{map[string]any{"type": "gamepad", "id": "g", "actions": []any{}}}}},
		{"missing id", map[string]any{"actions": []any{map[string]any{"type": "key", "actions": []any{}}}}},
		{"duplicate id", map[string]any{"actions": []any{keys("k"), keys("k")}}},
		{"bad pointerType", map[string]any{"actions": []any{map[string]any{"type": "pointer", "id": "p", "parameters": map[string]any{"pointerType": "stylus"}, "actions": []any{}}}}},
		{"key action on pointer", map[string]any{"actions": []any{mouse("m", keyDown("a"))}}},
		{"pointer action on key", map[string]any{"actions": []any{keys("k", down(0))}}},
		{"empty key value", map[string]any{"actions": []any{keys("k", keyDown(""))}}},
		{"negative button", map[string]any{"actions": []any{mouse("m", down(-1))}}},
		{"missing button", map[string]any{"actions": []any{mouse("m", map[string]any{"type": "pointerUp"})}}},
		{"negative duration", map[string]any{"actions": []any{keys("k", pause(-5))}}},
		{"bad origin", map[string]any{"actions": []any{mouse("m", map[string]any{"type": "pointerMove", "origin": "page"})}}},
		{"pointer origin on scroll", map[string]any{"actions": []any{map[string]any{"type": "wheel", "id": "w", "actions": []any{map[string]any{"type": "scroll", "origin": "pointer"}}}}}},
		{"pressure out of range", map[string]any{"actions": []any{mouse("m", map[string]any{"type": "pointerDown", "button": 0, "pressure": 2})}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, sid, ft := actionsSession(t)
			code, out := do(t, h, "POST", "/session/"+sid+"/actions", tt.body)
			if code != 400 || errCode(out) != ErrInvalidArgument {
				t.Fatalf("%d %v", code, out)
			}
			if evs := ft.Events(); len(evs) != 0 {
				t.Fatalf("invalid body dispatched input: %v", evs)
			}
		})
	}
}

func TestActionsSourceTypeIsSticky(t *testing.T) {
	h, sid, _ := actionsSession(t)
	perform(t, h, sid, keys("dev", pause(0)))
	_, out := do(t, h, "POST", "/session/"+sid+"/actions", map[string]any{"actions": []any{mouse("dev", pause(0))}})
	if errCode(out) != ErrInvalidArgument {
		t.Fatalf("type change accepted: %v", out)
	}
	perform(t, h, sid, mouse("m", pause(0)))
	_, out = do(t, h, "POST", "/session/"+sid+"/actions", map[string]any{"actions": []any{touch("m", pause(0))}})
	if errCode(out) != ErrInvalidArgument {
		t.Fatalf("pointerType change accepted: %v", out)
	}
	// Release resets the table, so the id is free again.
	do(t, h, "DELETE", "/session/"+sid+"/actions", nil)
	perform(t, h, sid, mouse("dev", pause(0)))
}

func TestActionsMouseClick(t *testing.T) {
	h, sid, ft := actionsSession(t)
	perform(t, h, sid, mouse("m", move(10, 20), down(0), up(0)))
	got := mouseEvents(ft.Events())
	want := []cdp.MouseEvent{
		{Type: "mouseMoved", X: 10, Y: 20, Button: "none", PointerType: "mouse"},
		{Type: "mousePressed", X: 10, Y: 20, Button: "left", Buttons: 1, ClickCount: 1, PointerType: "mouse"},
		{Type: "mouseReleased", X: 10, Y: 20, Button: "left", ClickCount: 1, PointerType: "mouse"},
	}
	if len(got) != len(want) {
		t.Fatalf("events = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestActionsDoubleAndContextClick(t *testing.T) {
	h, sid, ft := actionsSession(t)
	perform(t, h, sid, mouse("m", move(5, 5), down(0), up(0), down(0), up(0), down(2), up(2)))
	var presses []cdp.MouseEvent
	for _, e := range mouseEvents(ft.Events()) {
		if e.Type == "mousePressed" {
			presses = append(presses, e)
		}
	}
	if len(presses) != 3 || presses[0].ClickCount != 1 || presses[1].ClickCount != 2 {
		t.Fatalf("presses = %+v", presses)
	}
	if presses[2].Button != "right" || presses[2].Buttons != 2 || presses[2].ClickCount != 1 {
		t.Fatalf("right click = %+v", presses[2])
	}
}

func TestActionsDragHoldsButton(t *testing.T) {
	h, sid, ft := actionsSession(t)
	perform(t, h, sid, mouse("m", move(10, 10), down(0), move(100, 100), up(0)))
	evs := mouseEvents(ft.Events())
	drag := evs[2]
	if drag.Type != "mouseMoved" || drag.Button != "left" || drag.Buttons != 1 || drag.X != 100 {
		t.Fatalf("drag move = %+v", drag)
	}
}

func TestActionsModifiersAndShift(t *testing.T) {
	h, sid, ft := actionsSession(t)
	perform(t, h, sid,
		keys("kb", keyDown(""), pause(0), keyUp(""), keyDown(""), keyDown("a"), keyUp("a"), keyDown("1"), keyUp("")),
		mouse("m", move(1, 1), down(0)),
	)
	var keysSeen []cdp.KeyEvent
	var press cdp.MouseEvent
	for _, e := range ft.Events() {
		switch v := e.(type) {
		case cdp.KeyEvent:
			keysSeen = append(keysSeen, v)
		case cdp.MouseEvent:
			if v.Type == "mousePressed" {
				press = v
			}
		}
	}
	if press.Modifiers != modCtrl {
		t.Fatalf("ctrl-click modifiers = %d", press.Modifiers)
	}
	find := func(typ, key string) *cdp.KeyEvent {
		for i := range keysSeen {
			if keysSeen[i].Type == typ && keysSeen[i].Key == key {
				return &keysSeen[i]
			}
		}
		return nil
	}
	if a := find("keyDown", "A"); a == nil || a.Text != "A" || a.Code != "KeyA" || a.KeyCode != 65 || a.Modifiers != modShift {
		t.Fatalf("shift+a = %+v (all %+v)", a, keysSeen)
	}
	if bang := find("keyDown", "!"); bang == nil || bang.Code != "Digit1" {
		t.Fatalf("shift+1 = %+v", bang)
	}
	if up := find("keyUp", "Shift"); up == nil || up.Modifiers != 0 {
		t.Fatalf("shift up = %+v", up)
	}
	// "1" is still held; the mouse button too. Release lets go of both,
	// newest source first.
	before := len(ft.Events())
	if code, out := do(t, h, "DELETE", "/session/"+sid+"/actions", nil); code != 200 {
		t.Fatalf("release: %d %v", code, out)
	}
	rel := ft.Events()[before:]
	if len(rel) != 2 {
		t.Fatalf("release events = %+v", rel)
	}
	if m, ok := rel[0].(cdp.MouseEvent); !ok || m.Type != "mouseReleased" {
		t.Fatalf("first release = %+v", rel[0])
	}
	if k, ok := rel[1].(cdp.KeyEvent); !ok || k.Type != "keyUp" || k.Key != "1" {
		t.Fatalf("second release = %+v", rel[1])
	}
	// Nothing left to release.
	do(t, h, "DELETE", "/session/"+sid+"/actions", nil)
	if n := len(ft.Events()); n != before+2 {
		t.Fatalf("second release dispatched %d more events", n-before-2)
	}
}

func TestActionsCtrlChordHasNoText(t *testing.T) {
	h, sid, ft := actionsSession(t)
	perform(t, h, sid, keys("kb", keyDown(""), keyDown("a"), keyUp("a"), keyUp("")))
	k := ft.Events()[1].(cdp.KeyEvent)
	if k.Type != "rawKeyDown" || k.Text != "" || k.Modifiers != modCtrl {
		t.Fatalf("ctrl+a = %+v", k)
	}
}

func TestActionsTicksInterleave(t *testing.T) {
	h, sid, ft := actionsSession(t)
	perform(t, h, sid,
		keys("kb", keyDown("x"), keyUp("x")),
		mouse("m", pause(0), down(0)),
	)
	var order []string
	for _, e := range ft.Events() {
		switch v := e.(type) {
		case cdp.KeyEvent:
			order = append(order, v.Type)
		case cdp.MouseEvent:
			order = append(order, v.Type)
		}
	}
	want := []string{"keyDown", "keyUp", "mousePressed"}
	if len(order) != len(want) {
		t.Fatalf("order = %v", order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestActionsAnimatedMoveAndPause(t *testing.T) {
	h, sid, ft := actionsSession(t)
	start := time.Now()
	perform(t, h, sid, mouse("m", map[string]any{"type": "pointerMove", "x": 300, "y": 200, "duration": 80}, pause(40)))
	if el := time.Since(start); el < 120*time.Millisecond {
		t.Fatalf("move+pause took %v, want at least 120ms", el)
	}
	evs := mouseEvents(ft.Events())
	if len(evs) < 2 {
		t.Fatalf("animated move dispatched %d events", len(evs))
	}
	if last := evs[len(evs)-1]; last.X != 300 || last.Y != 200 {
		t.Fatalf("last = %+v", last)
	}
	for i := 1; i < len(evs); i++ {
		if evs[i].X < evs[i-1].X {
			t.Fatalf("move went backwards: %+v", evs)
		}
	}
}

func TestActionsPointerOriginAndBounds(t *testing.T) {
	h, sid, ft := actionsSession(t)
	perform(t, h, sid, mouse("m", move(100, 100), map[string]any{"type": "pointerMove", "origin": "pointer", "x": -10, "y": 5}))
	evs := mouseEvents(ft.Events())
	if last := evs[len(evs)-1]; last.X != 90 || last.Y != 105 {
		t.Fatalf("relative move = %+v", last)
	}
	for _, xy := range [][2]float64{{-1, 0}, {0, 601}, {801, 10}} {
		code, out := do(t, h, "POST", "/session/"+sid+"/actions", map[string]any{"actions": []any{mouse("m", move(xy[0], xy[1]))}})
		if code != 500 || errCode(out) != ErrMoveTargetOutOfBounds {
			t.Fatalf("%v: %d %v", xy, code, out)
		}
	}
}

func TestActionsElementOriginAndWheel(t *testing.T) {
	h, sid, ft := actionsSession(t)
	ft.ScriptResult = map[string]any{"x": 200, "y": 150}
	el := map[string]any{ElementKey: "e5"}
	perform(t, h, sid,
		mouse("m", map[string]any{"type": "pointerMove", "origin": el, "x": 3, "y": 4}),
		map[string]any{"type": "wheel", "id": "w", "actions": []any{
			pause(0),
			map[string]any{"type": "scroll", "origin": el, "x": 0, "y": 0, "deltaX": 0, "deltaY": 250},
		}},
	)
	evs := mouseEvents(ft.Events())
	if len(evs) != 2 {
		t.Fatalf("events = %+v", evs)
	}
	if evs[0].Type != "mouseMoved" || evs[0].X != 203 || evs[0].Y != 154 {
		t.Fatalf("element move = %+v", evs[0])
	}
	if evs[1].Type != "mouseWheel" || evs[1].X != 200 || evs[1].Y != 150 || evs[1].DeltaY != 250 {
		t.Fatalf("wheel = %+v", evs[1])
	}
}

func TestActionsAnimatedScrollSumsToDelta(t *testing.T) {
	h, sid, ft := actionsSession(t)
	perform(t, h, sid, map[string]any{"type": "wheel", "id": "w", "actions": []any{
		map[string]any{"type": "scroll", "x": 10, "y": 10, "deltaX": 33, "deltaY": -101, "duration": 60},
	}})
	var dx, dy float64
	evs := mouseEvents(ft.Events())
	for _, e := range evs {
		dx += e.DeltaX
		dy += e.DeltaY
	}
	if len(evs) < 2 || dx != 33 || dy != -101 {
		t.Fatalf("%d wheel events summing to (%g, %g)", len(evs), dx, dy)
	}
}

func TestActionsMultiTouch(t *testing.T) {
	h, sid, ft := actionsSession(t)
	perform(t, h, sid,
		touch("f1", move(10, 10), down(0), move(20, 10), up(0)),
		touch("f2", move(50, 50), down(0), pause(0), pause(0), up(0)),
	)
	var got []string
	for _, e := range ft.Events() {
		te := e.(cdp.TouchEvent)
		got = append(got, te.Type+":"+string(rune('0'+len(te.TouchPoints))))
	}
	want := []string{"touchStart:1", "touchStart:2", "touchMove:2", "touchMove:1", "touchEnd:0"}
	if len(got) != len(want) {
		t.Fatalf("touch events = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("touch events = %v, want %v", got, want)
		}
	}
}

func TestReleaseActionsWithoutState(t *testing.T) {
	h, sid, ft := actionsSession(t)
	if code, out := do(t, h, "DELETE", "/session/"+sid+"/actions", nil); code != 200 || out["value"] != nil {
		t.Fatalf("%d %v", code, out)
	}
	if len(ft.Events()) != 0 {
		t.Fatal("release with no state dispatched input")
	}
}
