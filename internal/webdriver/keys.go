package webdriver

import (
	"context"

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
	'': {"Backspace", "Backspace", 8, ""},
	'': {"Tab", "Tab", 9, ""},
	'': {"Clear", "NumLock", 12, ""},
	'': {"Enter", "Enter", 13, "\r"},
	'': {"Enter", "Enter", 13, "\r"},
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
}

// Modifier keys toggle (they are sticky within one send-keys call) and
// map to CDP modifier bits.
var modifierKeys = map[rune]struct {
	def keyDef
	bit int
}{
	'': {keyDef{"Alt", "AltLeft", 18, ""}, 1},
	'': {keyDef{"Control", "ControlLeft", 17, ""}, 2},
	'': {keyDef{"Meta", "MetaLeft", 91, ""}, 4},
	'': {keyDef{"Shift", "ShiftLeft", 16, ""}, 8},
}

const keyNull = ''

// typeText sends text as real key events to the focused element.
func typeText(ctx context.Context, tab cdp.Tab, text string) error {
	mods := 0
	var held []keyDef
	release := func() error {
		for i := len(held) - 1; i >= 0; i-- {
			if err := tab.Key(ctx, cdp.KeyEvent{Type: "keyUp", Key: held[i].key, Code: held[i].code, KeyCode: held[i].vk}); err != nil {
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
		if m, ok := modifierKeys[r]; ok {
			if mods&m.bit != 0 {
				continue
			}
			mods |= m.bit
			held = append(held, m.def)
			if err := tab.Key(ctx, cdp.KeyEvent{Type: "rawKeyDown", Key: m.def.key, Code: m.def.code, KeyCode: m.def.vk, Modifiers: mods}); err != nil {
				return err
			}
			continue
		}
		d, ok := specialKeys[r]
		if !ok {
			d = keyDef{key: string(r), text: string(r)}
		}
		text := d.text
		if mods&(2|4) != 0 { // Ctrl/Meta chords produce no text
			text = ""
		}
		down := cdp.KeyEvent{Type: "keyDown", Key: d.key, Code: d.code, KeyCode: d.vk, Text: text, Modifiers: mods}
		if text == "" {
			down.Type = "rawKeyDown"
		}
		if err := tab.Key(ctx, down); err != nil {
			return err
		}
		if err := tab.Key(ctx, cdp.KeyEvent{Type: "keyUp", Key: d.key, Code: d.code, KeyCode: d.vk, Modifiers: mods}); err != nil {
			return err
		}
	}
	return release()
}
