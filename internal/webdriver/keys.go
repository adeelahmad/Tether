package webdriver

import (
	"context"
	"unicode/utf8"

	"github.com/adeelahmad/tether/internal/cdp"
)

type keyDef struct {
	key  string
	code string
	vk   int
	text string
}

// W3C normalized keys (https://www.w3.org/TR/webdriver2/#keyboard-actions).
var specialKeys = map[rune]keyDef{
	'': {"Cancel", "", 3, ""},
	'': {"Help", "Help", 6, ""},
	'': {"Backspace", "Backspace", 8, ""},
	'': {"Tab", "Tab", 9, ""},
	'': {"Clear", "NumLock", 12, ""},
	'': {"Enter", "Enter", 13, "\r"},
	'': {"Enter", "NumpadEnter", 13, "\r"},
	'': {"Pause", "Pause", 19, ""},
	'': {"Escape", "Escape", 27, ""},
	'': {" ", "Space", 32, " "},
	'': {"PageUp", "PageUp", 33, ""},
	'': {"PageDown", "PageDown", 34, ""},
	'': {"End", "End", 35, ""},
	'': {"Home", "Home", 36, ""},
	'': {"ArrowLeft", "ArrowLeft", 37, ""},
	'': {"ArrowUp", "ArrowUp", 38, ""},
	'': {"ArrowRight", "ArrowRight", 39, ""},
	'': {"ArrowDown", "ArrowDown", 40, ""},
	'': {"Insert", "Insert", 45, ""},
	'': {"Delete", "Delete", 46, ""},
	'': {";", "Semicolon", 186, ";"},
	'': {"=", "Equal", 187, "="},
	'': {"0", "Numpad0", 96, "0"},
	'': {"1", "Numpad1", 97, "1"},
	'': {"2", "Numpad2", 98, "2"},
	'': {"3", "Numpad3", 99, "3"},
	'': {"4", "Numpad4", 100, "4"},
	'': {"5", "Numpad5", 101, "5"},
	'': {"6", "Numpad6", 102, "6"},
	'': {"7", "Numpad7", 103, "7"},
	'': {"8", "Numpad8", 104, "8"},
	'': {"9", "Numpad9", 105, "9"},
	'': {"*", "NumpadMultiply", 106, "*"},
	'': {"+", "NumpadAdd", 107, "+"},
	'': {",", "NumpadComma", 108, ","},
	'': {"-", "NumpadSubtract", 109, "-"},
	'': {".", "NumpadDecimal", 110, "."},
	'': {"/", "NumpadDivide", 111, "/"},
	'': {"F1", "F1", 112, ""},
	'': {"F2", "F2", 113, ""},
	'': {"F3", "F3", 114, ""},
	'': {"F4", "F4", 115, ""},
	'': {"F5", "F5", 116, ""},
	'': {"F6", "F6", 117, ""},
	'': {"F7", "F7", 118, ""},
	'': {"F8", "F8", 119, ""},
	'': {"F9", "F9", 120, ""},
	'': {"F10", "F10", 121, ""},
	'': {"F11", "F11", 122, ""},
	'': {"F12", "F12", 123, ""},
	'': {"ZenkakuHankaku", "Lang5", 0, ""},
	'': {"PageUp", "Numpad9", 33, ""},
	'': {"PageDown", "Numpad3", 34, ""},
	'': {"End", "Numpad1", 35, ""},
	'': {"Home", "Numpad7", 36, ""},
	'': {"ArrowLeft", "Numpad4", 37, ""},
	'': {"ArrowUp", "Numpad8", 38, ""},
	'': {"ArrowRight", "Numpad6", 39, ""},
	'': {"ArrowDown", "Numpad2", 40, ""},
	'': {"Insert", "Numpad0", 45, ""},
	'': {"Delete", "NumpadDecimal", 46, ""},
}

// CDP modifier bits.
const (
	modAlt   = 1
	modCtrl  = 2
	modMeta  = 4
	modShift = 8
)

// Modifier keys toggle (they are sticky within one send-keys call) and
// map to CDP modifier bits.
var modifierKeys = map[rune]struct {
	def keyDef
	bit int
}{
	'': {keyDef{"Alt", "AltLeft", 18, ""}, modAlt},
	'': {keyDef{"Control", "ControlLeft", 17, ""}, modCtrl},
	'': {keyDef{"Meta", "MetaLeft", 91, ""}, modMeta},
	'': {keyDef{"Shift", "ShiftLeft", 16, ""}, modShift},
	'': {keyDef{"Alt", "AltRight", 18, ""}, modAlt},
	'': {keyDef{"Control", "ControlRight", 17, ""}, modCtrl},
	'': {keyDef{"Meta", "MetaRight", 92, ""}, modMeta},
	'': {keyDef{"Shift", "ShiftRight", 16, ""}, modShift},
}

const keyNull = ''

// charKey describes a printable character, filling in the physical key
// code and virtual key code for letters and digits so page handlers that
// look at event.code or event.keyCode see what a real keyboard sends.
func charKey(s string) keyDef {
	d := keyDef{key: s, text: s}
	if r, n := utf8.DecodeRuneInString(s); n == len(s) {
		switch {
		case r >= 'a' && r <= 'z':
			d.code, d.vk = "Key"+string(r-'a'+'A'), int(r-'a'+'A')
		case r >= 'A' && r <= 'Z':
			d.code, d.vk = "Key"+string(r), int(r)
		case r >= '0' && r <= '9':
			d.code, d.vk = "Digit"+string(r), int(r)
		}
	}
	return d
}

// lookupKey resolves one key value. bit is the CDP modifier bit for
// modifier keys and 0 otherwise.
func lookupKey(s string) (d keyDef, bit int) {
	if r, n := utf8.DecodeRuneInString(s); n == len(s) {
		if m, ok := modifierKeys[r]; ok {
			return m.def, m.bit
		}
		if d, ok := specialKeys[r]; ok {
			return d, 0
		}
	}
	return charKey(s), 0
}

// keyDownEvent is the CDP event for pressing d with mods held.
func keyDownEvent(d keyDef, mods int) cdp.KeyEvent {
	text := d.text
	if mods&(modCtrl|modMeta) != 0 { // Ctrl/Meta chords produce no text
		text = ""
	}
	ev := cdp.KeyEvent{Type: "keyDown", Key: d.key, Code: d.code, KeyCode: d.vk, Text: text, Modifiers: mods}
	if text == "" {
		ev.Type = "rawKeyDown"
	}
	return ev
}

func keyUpEvent(d keyDef, mods int) cdp.KeyEvent {
	return cdp.KeyEvent{Type: "keyUp", Key: d.key, Code: d.code, KeyCode: d.vk, Modifiers: mods}
}

// typeText sends text as real key events to the focused element.
func typeText(ctx context.Context, tab cdp.Tab, text string) error {
	mods := 0
	var held []keyDef
	release := func() error {
		for i := len(held) - 1; i >= 0; i-- {
			if err := tab.Key(ctx, keyUpEvent(held[i], 0)); err != nil {
				return err
			}
		}
		held, mods = nil, 0
		return nil
	}
	for _, r := range text {
		if r == keyNull {
			if err := release(); err != nil {
				return err
			}
			continue
		}
		d, bit := lookupKey(string(r))
		if bit != 0 {
			if mods&bit != 0 {
				continue
			}
			mods |= bit
			held = append(held, d)
			if err := tab.Key(ctx, cdp.KeyEvent{Type: "rawKeyDown", Key: d.key, Code: d.code, KeyCode: d.vk, Modifiers: mods}); err != nil {
				return err
			}
			continue
		}
		if err := tab.Key(ctx, keyDownEvent(d, mods)); err != nil {
			return err
		}
		if err := tab.Key(ctx, keyUpEvent(d, mods)); err != nil {
			return err
		}
	}
	return release()
}
