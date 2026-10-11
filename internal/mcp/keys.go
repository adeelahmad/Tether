package mcp

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/adeelahmad/tether/internal/cdp"
)

type keyDef struct {
	key  string
	code string
	vk   int
	text string
}

// namedKeys are the DOM key names agents use ("Enter", "ArrowDown", …),
// matched case-insensitively.
var namedKeys = map[string]keyDef{
	"enter":      {"Enter", "Enter", 13, "\r"},
	"return":     {"Enter", "Enter", 13, "\r"},
	"tab":        {"Tab", "Tab", 9, ""},
	"escape":     {"Escape", "Escape", 27, ""},
	"esc":        {"Escape", "Escape", 27, ""},
	"backspace":  {"Backspace", "Backspace", 8, ""},
	"delete":     {"Delete", "Delete", 46, ""},
	"insert":     {"Insert", "Insert", 45, ""},
	"space":      {" ", "Space", 32, " "},
	" ":          {" ", "Space", 32, " "},
	"arrowup":    {"ArrowUp", "ArrowUp", 38, ""},
	"arrowdown":  {"ArrowDown", "ArrowDown", 40, ""},
	"arrowleft":  {"ArrowLeft", "ArrowLeft", 37, ""},
	"arrowright": {"ArrowRight", "ArrowRight", 39, ""},
	"up":         {"ArrowUp", "ArrowUp", 38, ""},
	"down":       {"ArrowDown", "ArrowDown", 40, ""},
	"left":       {"ArrowLeft", "ArrowLeft", 37, ""},
	"right":      {"ArrowRight", "ArrowRight", 39, ""},
	"home":       {"Home", "Home", 36, ""},
	"end":        {"End", "End", 35, ""},
	"pageup":     {"PageUp", "PageUp", 33, ""},
	"pagedown":   {"PageDown", "PageDown", 34, ""},
	"f1":         {"F1", "F1", 112, ""},
	"f2":         {"F2", "F2", 113, ""},
	"f3":         {"F3", "F3", 114, ""},
	"f4":         {"F4", "F4", 115, ""},
	"f5":         {"F5", "F5", 116, ""},
	"f6":         {"F6", "F6", 117, ""},
	"f7":         {"F7", "F7", 118, ""},
	"f8":         {"F8", "F8", 119, ""},
	"f9":         {"F9", "F9", 120, ""},
	"f10":        {"F10", "F10", 121, ""},
	"f11":        {"F11", "F11", 122, ""},
	"f12":        {"F12", "F12", 123, ""},
}

// Modifier names and their CDP modifier bits.
var modifiers = map[string]struct {
	def keyDef
	bit int
}{
	"alt":     {keyDef{"Alt", "AltLeft", 18, ""}, 1},
	"option":  {keyDef{"Alt", "AltLeft", 18, ""}, 1},
	"control": {keyDef{"Control", "ControlLeft", 17, ""}, 2},
	"ctrl":    {keyDef{"Control", "ControlLeft", 17, ""}, 2},
	"meta":    {keyDef{"Meta", "MetaLeft", 91, ""}, 4},
	"cmd":     {keyDef{"Meta", "MetaLeft", 91, ""}, 4},
	"command": {keyDef{"Meta", "MetaLeft", 91, ""}, 4},
	"shift":   {keyDef{"Shift", "ShiftLeft", 16, ""}, 8},
}

// charKey describes a printable character.
func charKey(r rune) keyDef {
	s := string(r)
	d := keyDef{key: s, text: s}
	switch {
	case r >= 'a' && r <= 'z':
		d.code, d.vk = "Key"+strings.ToUpper(s), int(r-'a'+'A')
	case r >= 'A' && r <= 'Z':
		d.code, d.vk = "Key"+s, int(r)
	case r >= '0' && r <= '9':
		d.code, d.vk = "Digit"+s, int(r)
	}
	return d
}

// parseChord parses "Enter", "a", "Control+A" or "Shift+Tab" into the
// held modifiers and the key itself.
func parseChord(chord string) ([]keyDef, int, keyDef, error) {
	if chord == "" {
		return nil, 0, keyDef{}, badArgs("key is required, e.g. Enter, Tab, ArrowDown or Control+A")
	}
	parts := []string{chord}
	if chord != "+" && strings.Contains(chord, "+") {
		parts = strings.Split(chord, "+")
		if strings.HasSuffix(chord, "++") { // "Control++"
			parts = append(parts[:len(parts)-2], "+")
		}
	}
	var held []keyDef
	mods := 0
	for _, p := range parts[:len(parts)-1] {
		m, ok := modifiers[strings.ToLower(strings.TrimSpace(p))]
		if !ok {
			return nil, 0, keyDef{}, badArgs("unknown modifier %q (use Control, Shift, Alt or Meta)", p)
		}
		if mods&m.bit == 0 {
			mods |= m.bit
			held = append(held, m.def)
		}
	}
	last := parts[len(parts)-1]
	if d, ok := namedKeys[strings.ToLower(last)]; ok {
		return held, mods, d, nil
	}
	if m, ok := modifiers[strings.ToLower(last)]; ok {
		return held, mods, m.def, nil
	}
	if utf8.RuneCountInString(last) == 1 {
		r, _ := utf8.DecodeRuneInString(last)
		return held, mods, charKey(r), nil
	}
	return nil, 0, keyDef{}, badArgs("unknown key %q", last)
}

// pressChord dispatches one key chord as real key events.
func pressChord(ctx context.Context, tab cdp.Tab, chord string) error {
	held, mods, d, err := parseChord(chord)
	if err != nil {
		return err
	}
	cur := 0
	for _, m := range held {
		cur |= modifiers[strings.ToLower(m.key)].bit
		if err := tab.Key(ctx, cdp.KeyEvent{Type: "rawKeyDown", Key: m.key, Code: m.code, KeyCode: m.vk, Modifiers: cur}); err != nil {
			return err
		}
	}
	if err := stroke(ctx, tab, d, mods); err != nil {
		return err
	}
	for i := len(held) - 1; i >= 0; i-- {
		m := held[i]
		cur &^= modifiers[strings.ToLower(m.key)].bit
		if err := tab.Key(ctx, cdp.KeyEvent{Type: "keyUp", Key: m.key, Code: m.code, KeyCode: m.vk, Modifiers: cur}); err != nil {
			return err
		}
	}
	return nil
}

// stroke sends keyDown/keyUp for d. Ctrl/Meta chords produce no text, so
// shortcuts like Control+A act instead of typing "a".
func stroke(ctx context.Context, tab cdp.Tab, d keyDef, mods int) error {
	text := d.text
	if mods&(2|4) != 0 {
		text = ""
	}
	down := cdp.KeyEvent{Type: "keyDown", Key: d.key, Code: d.code, KeyCode: d.vk, Text: text, Modifiers: mods}
	if text == "" {
		down.Type = "rawKeyDown"
	}
	if err := tab.Key(ctx, down); err != nil {
		return err
	}
	return tab.Key(ctx, cdp.KeyEvent{Type: "keyUp", Key: d.key, Code: d.code, KeyCode: d.vk, Modifiers: mods})
}

// typeText types text into the focused element as real key events, so
// pages see the same input events a person would produce. Newlines press
// Enter.
func typeText(ctx context.Context, tab cdp.Tab, text string) error {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	for _, r := range text {
		d := charKey(r)
		switch r {
		case '\n', '\r':
			d = namedKeys["enter"]
		case '\t':
			d = namedKeys["tab"]
		}
		if err := stroke(ctx, tab, d, 0); err != nil {
			return err
		}
	}
	return nil
}
