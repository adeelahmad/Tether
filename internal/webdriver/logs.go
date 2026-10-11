package webdriver

import (
	"context"
	"net/http"
	"strconv"

	"github.com/adeelahmad/tether/internal/cdp"
)

// logTypes are the log types the legacy Selenium/ChromeDriver log endpoint
// accepts. "browser" is the tab's console; Tether has no driver log of its
// own, so "driver" is always empty.
var logTypes = []string{"browser", "driver"}

func (h *Handler) getLogTypes(context.Context, *wdSession, *http.Request, []byte) (any, error) {
	return logTypes, nil
}

// wdLogEntry is a ChromeDriver-style log entry.
type wdLogEntry struct {
	Level     string `json:"level"`
	Message   string `json:"message"`
	Source    string `json:"source"`
	Timestamp int64  `json:"timestamp"`
}

// getLog returns the entries logged since the previous call, as
// ChromeDriver does: each entry is returned once per WebDriver session.
func (h *Handler) getLog(_ context.Context, ws *wdSession, _ *http.Request, body []byte) (any, error) {
	var req struct {
		Type string `json:"type"`
	}
	if err := decodeBody(body, &req); err != nil {
		return nil, err
	}
	switch req.Type {
	case "browser":
	case "driver":
		return []wdLogEntry{}, nil
	default:
		return nil, wdErr(ErrInvalidArgument, "unknown log type "+strconv.Quote(req.Type)+"; supported: browser, driver")
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	entries, next := h.core.Logs(ws.s, ws.logCursor, "")
	ws.logCursor = next
	out := make([]wdLogEntry, len(entries))
	for i, e := range entries {
		out[i] = toWDLog(e)
	}
	return out, nil
}

func toWDLog(e cdp.LogEntry) wdLogEntry {
	msg := e.Text
	if e.URL != "" {
		// ChromeDriver prefixes the location, "<url> <line>:<col> <text>";
		// Tether has no column.
		loc := e.URL
		if e.Line > 0 {
			loc += " " + strconv.Itoa(e.Line)
		}
		msg = loc + " " + e.Text
	}
	source := e.Source
	switch source {
	case "console":
		source = "console-api"
	case "exception":
		source = "javascript"
	}
	return wdLogEntry{Level: wdLevel(e.Level), Message: msg, Source: source, Timestamp: e.Time.UnixMilli()}
}

// wdLevel maps onto java.util.logging level names, which Selenium clients
// expect.
func wdLevel(level string) string {
	switch level {
	case cdp.LevelDebug:
		return "DEBUG"
	case cdp.LevelWarning:
		return "WARNING"
	case cdp.LevelError:
		return "SEVERE"
	}
	return "INFO"
}
