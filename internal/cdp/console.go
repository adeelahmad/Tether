package cdp

import (
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// LogEntry is one console message, uncaught exception or browser log entry
// seen in a tab.
type LogEntry struct {
	// Seq increases by one per entry in a tab; pass the last Seq you saw to
	// Tab.Logs to read only newer entries.
	Seq  uint64    `json:"seq"`
	Time time.Time `json:"time"`
	// Source is "console" (console.* calls), "exception" (uncaught errors)
	// or the Log domain's source: "network", "security", "violation", …
	Source string `json:"source"`
	// Level is one of debug, info, warning, error.
	Level string `json:"level"`
	Text  string `json:"text"`
	URL   string `json:"url,omitempty"`
	Line  int    `json:"line,omitempty"` // 1-based; 0 when unknown
}

// Log levels, ordered by severity.
const (
	LevelDebug   = "debug"
	LevelInfo    = "info"
	LevelWarning = "warning"
	LevelError   = "error"
)

// LevelRank orders levels by severity; unknown levels rank as info.
func LevelRank(level string) int {
	switch level {
	case LevelDebug:
		return 0
	case LevelWarning:
		return 2
	case LevelError:
		return 3
	}
	return 1
}

// DefaultLogBuffer is how many log entries each tab keeps.
const DefaultLogBuffer = 1000

// maxLogText bounds one entry so a page logging megabytes cannot balloon
// Tether's memory.
const maxLogText = 8 << 10

// logBuffer is a per-tab ring of the most recent log entries. It is fed
// from the dispatcher goroutine.
type logBuffer struct {
	mu   sync.Mutex
	buf  []LogEntry
	size int
	next uint64 // Seq of the next entry
}

func newLogBuffer(size int) *logBuffer {
	return &logBuffer{size: size, next: 1}
}

func (b *logBuffer) add(e LogEntry) {
	if len(e.Text) > maxLogText {
		e.Text = e.Text[:maxLogText] + "…"
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e.Seq = b.next
	b.next++
	if len(b.buf) >= b.size {
		copy(b.buf, b.buf[1:])
		b.buf = b.buf[:len(b.buf)-1]
	}
	b.buf = append(b.buf, e)
}

// since returns a copy of the entries with Seq > after.
func (b *logBuffer) since(after uint64) []LogEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	i := len(b.buf)
	for i > 0 && b.buf[i-1].Seq > after {
		i--
	}
	return append([]LogEntry(nil), b.buf[i:]...)
}

// handle turns Runtime and Log events into entries.
func (b *logBuffer) handle(e Event) {
	switch e.Method {
	case "Runtime.consoleAPICalled":
		var p struct {
			Type       string         `json:"type"`
			Args       []remoteObject `json:"args"`
			Timestamp  float64        `json:"timestamp"`
			StackTrace *stackTrace    `json:"stackTrace"`
		}
		if json.Unmarshal(e.Params, &p) != nil {
			return
		}
		parts := make([]string, len(p.Args))
		for i, a := range p.Args {
			parts[i] = formatArg(a)
		}
		le := LogEntry{Time: msTime(p.Timestamp), Source: "console", Level: consoleLevel(p.Type), Text: strings.Join(parts, " ")}
		le.URL, le.Line = p.StackTrace.top()
		b.add(le)
	case "Runtime.exceptionThrown":
		var p struct {
			Timestamp        float64 `json:"timestamp"`
			ExceptionDetails struct {
				exceptionDetails
				URL        string      `json:"url"`
				LineNumber int         `json:"lineNumber"`
				StackTrace *stackTrace `json:"stackTrace"`
			} `json:"exceptionDetails"`
		}
		if json.Unmarshal(e.Params, &p) != nil {
			return
		}
		d := p.ExceptionDetails
		le := LogEntry{Time: msTime(p.Timestamp), Source: "exception", Level: LevelError, Text: d.message()}
		le.URL, le.Line = d.StackTrace.top()
		if le.URL == "" && d.URL != "" {
			le.URL, le.Line = d.URL, d.LineNumber+1
		}
		b.add(le)
	case "Log.entryAdded":
		var p struct {
			Entry struct {
				Source     string  `json:"source"`
				Level      string  `json:"level"`
				Text       string  `json:"text"`
				URL        string  `json:"url"`
				LineNumber *int    `json:"lineNumber"`
				Timestamp  float64 `json:"timestamp"`
			} `json:"entry"`
		}
		if json.Unmarshal(e.Params, &p) != nil || p.Entry.Source == "console-api" {
			return // console-api duplicates Runtime.consoleAPICalled
		}
		le := LogEntry{Time: msTime(p.Entry.Timestamp), Source: p.Entry.Source, Level: logLevel(p.Entry.Level), Text: p.Entry.Text, URL: p.Entry.URL}
		if p.Entry.LineNumber != nil {
			le.Line = *p.Entry.LineNumber + 1
		}
		b.add(le)
	}
}

type stackTrace struct {
	CallFrames []struct {
		URL        string `json:"url"`
		LineNumber int    `json:"lineNumber"`
	} `json:"callFrames"`
}

func (s *stackTrace) top() (string, int) {
	if s == nil || len(s.CallFrames) == 0 {
		return "", 0
	}
	f := s.CallFrames[0]
	return f.URL, f.LineNumber + 1
}

// formatArg renders a console argument roughly as DevTools prints it:
// strings bare, primitives as JSON, objects by their description.
func formatArg(a remoteObject) string {
	switch {
	case a.UnserializableValue != "":
		return a.UnserializableValue
	case a.Type == "string":
		var s string
		if json.Unmarshal(a.Value, &s) == nil {
			return s
		}
	case a.Type == "undefined":
		return "undefined"
	case len(a.Value) > 0 && a.Type != "object" && a.Type != "function":
		return string(a.Value)
	case a.Subtype == "null":
		return "null"
	}
	if a.Description != "" {
		return a.Description
	}
	if len(a.Value) > 0 {
		return string(a.Value)
	}
	return a.Type
}

func consoleLevel(t string) string {
	switch t {
	case "debug":
		return LevelDebug
	case "warning":
		return LevelWarning
	case "error", "assert":
		return LevelError
	}
	return LevelInfo
}

func logLevel(l string) string {
	switch l {
	case "verbose":
		return LevelDebug
	case "warning":
		return LevelWarning
	case "error":
		return LevelError
	}
	return LevelInfo
}

// msTime converts a CDP Runtime.Timestamp (ms since the epoch).
func msTime(ms float64) time.Time {
	if ms <= 0 {
		return time.Now()
	}
	return time.UnixMilli(int64(ms)).Add(time.Duration((ms - float64(int64(ms))) * float64(time.Millisecond)))
}
