package cdp

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestLogBufferHandle(t *testing.T) {
	tests := []struct {
		name   string
		method string
		params string
		want   *LogEntry // nil = ignored
	}{
		{"console.log args", "Runtime.consoleAPICalled",
			`{"type":"log","timestamp":1700000000123.5,"args":[{"type":"string","value":"hi"},{"type":"number","value":42},{"type":"object","className":"Object","description":"Object"},{"type":"undefined"},{"type":"object","subtype":"null","value":null},{"type":"number","unserializableValue":"NaN"}],
			"stackTrace":{"callFrames":[{"url":"https://a.test/app.js","lineNumber":9}]}}`,
			&LogEntry{Source: "console", Level: LevelInfo, Text: "hi 42 Object undefined null NaN", URL: "https://a.test/app.js", Line: 10}},
		{"console.warn", "Runtime.consoleAPICalled", `{"type":"warning","args":[{"type":"string","value":"careful"}]}`,
			&LogEntry{Source: "console", Level: LevelWarning, Text: "careful"}},
		{"console.assert", "Runtime.consoleAPICalled", `{"type":"assert","args":[{"type":"string","value":"Assertion failed"}]}`,
			&LogEntry{Source: "console", Level: LevelError, Text: "Assertion failed"}},
		{"console.debug", "Runtime.consoleAPICalled", `{"type":"debug","args":[{"type":"boolean","value":true}]}`,
			&LogEntry{Source: "console", Level: LevelDebug, Text: "true"}},
		{"uncaught exception", "Runtime.exceptionThrown",
			`{"timestamp":1,"exceptionDetails":{"text":"Uncaught","url":"https://a.test/","lineNumber":2,"exception":{"type":"object","description":"Error: kaboom\n    at https://a.test/:3:7"}}}`,
			&LogEntry{Source: "exception", Level: LevelError, Text: "Error: kaboom\n    at https://a.test/:3:7", URL: "https://a.test/", Line: 3}},
		{"exception without object", "Runtime.exceptionThrown", `{"exceptionDetails":{"text":"Uncaught SyntaxError"}}`,
			&LogEntry{Source: "exception", Level: LevelError, Text: "Uncaught SyntaxError"}},
		{"network log", "Log.entryAdded",
			`{"entry":{"source":"network","level":"error","text":"Failed to load resource","url":"https://a.test/x.png","timestamp":1}}`,
			&LogEntry{Source: "network", Level: LevelError, Text: "Failed to load resource", URL: "https://a.test/x.png"}},
		{"verbose log", "Log.entryAdded", `{"entry":{"source":"violation","level":"verbose","text":"slow","lineNumber":0}}`,
			&LogEntry{Source: "violation", Level: LevelDebug, Text: "slow", Line: 1}},
		{"console-api duplicate", "Log.entryAdded", `{"entry":{"source":"console-api","level":"info","text":"dup"}}`, nil},
		{"unrelated event", "Page.loadEventFired", `{}`, nil},
		{"malformed", "Runtime.consoleAPICalled", `{"args":`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newLogBuffer(10)
			b.handle(Event{Method: tt.method, Params: []byte(tt.params)})
			got := b.since(0)
			if tt.want == nil {
				if len(got) != 0 {
					t.Fatalf("expected nothing, got %+v", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("got %d entries", len(got))
			}
			g := got[0]
			if g.Seq != 1 || g.Time.IsZero() {
				t.Fatalf("seq/time: %+v", g)
			}
			g.Seq, g.Time = 0, time.Time{}
			if g != *tt.want {
				t.Fatalf("got  %+v\nwant %+v", g, *tt.want)
			}
		})
	}
}

func TestLogBufferRing(t *testing.T) {
	b := newLogBuffer(3)
	for i := 1; i <= 5; i++ {
		b.add(LogEntry{Text: fmt.Sprint(i)})
	}
	got := b.since(0)
	if len(got) != 3 || got[0].Seq != 3 || got[2].Seq != 5 || got[2].Text != "5" {
		t.Fatalf("ring kept %+v", got)
	}
	if got := b.since(4); len(got) != 1 || got[0].Seq != 5 {
		t.Fatalf("since(4) = %+v", got)
	}
	if got := b.since(5); len(got) != 0 {
		t.Fatalf("since(5) = %+v", got)
	}
	// Callers get a copy.
	got[2].Text = "x"
	if b.since(4)[0].Text != "5" {
		t.Fatal("since returned the live buffer")
	}
}

func TestLogBufferTruncates(t *testing.T) {
	b := newLogBuffer(1)
	b.add(LogEntry{Text: strings.Repeat("a", maxLogText+100)})
	if n := len(b.since(0)[0].Text); n > maxLogText+4 {
		t.Fatalf("entry is %d bytes", n)
	}
}

func TestMsTime(t *testing.T) {
	if got := msTime(1700000000123.5); got.UnixMicro() != 1700000000123500 {
		t.Fatalf("msTime = %v", got.UnixMicro())
	}
	if msTime(0).IsZero() {
		t.Fatal("zero timestamp should fall back to now")
	}
}

func TestLevelRank(t *testing.T) {
	order := []string{LevelDebug, LevelInfo, LevelWarning, LevelError}
	for i := 1; i < len(order); i++ {
		if LevelRank(order[i-1]) >= LevelRank(order[i]) {
			t.Fatalf("%s should rank below %s", order[i-1], order[i])
		}
	}
	if LevelRank("weird") != LevelRank(LevelInfo) {
		t.Fatal("unknown level should rank as info")
	}
}
