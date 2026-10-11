package cdp

import (
	"context"
	"encoding/json"
	"time"
)

// BlockedByClient is the network error a guarded navigation fails with.
const BlockedByClient = "net::ERR_BLOCKED_BY_CLIENT"

// Guard implements Tab.
func (t *chromeTab) Guard(ctx context.Context, check func(string) error) error {
	t.guardMu.Lock()
	defer t.guardMu.Unlock()
	if check == nil {
		if t.guardUnsub == nil {
			return nil
		}
		t.guardUnsub()
		t.guardUnsub = nil
		t.guardCheck.Store(nil)
		return t.call(ctx, "Fetch.disable", nil, nil)
	}
	t.guardCheck.Store(&check)
	if t.guardUnsub != nil {
		return nil
	}
	unsub := t.b.c.subscribe(t.session, t.onRequestPaused)
	err := t.call(ctx, "Fetch.enable", map[string]any{
		"patterns": []map[string]any{{"urlPattern": "*", "resourceType": "Document", "requestStage": "Request"}},
	}, nil)
	if err != nil {
		unsub()
		t.guardCheck.Store(nil)
		return err
	}
	t.guardUnsub = unsub
	return nil
}

// onRequestPaused runs on the dispatcher goroutine, so it only decides and
// hands the reply to its own goroutine.
func (t *chromeTab) onRequestPaused(e Event) {
	if e.Method != "Fetch.requestPaused" {
		return
	}
	var p struct {
		RequestID string `json:"requestId"`
		FrameID   string `json:"frameId"`
		Request   struct {
			URL string `json:"url"`
		} `json:"request"`
	}
	if json.Unmarshal(e.Params, &p) != nil || p.RequestID == "" {
		return
	}
	// Only the top-level frame (whose ID is the target ID) is guarded:
	// sites legitimately embed third-party frames.
	check := t.guardCheck.Load()
	blocked := check != nil && p.FrameID == t.id && (*check)(p.Request.URL) != nil
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if blocked {
			_ = t.call(ctx, "Fetch.failRequest", map[string]any{"requestId": p.RequestID, "errorReason": "BlockedByClient"}, nil)
			return
		}
		_ = t.call(ctx, "Fetch.continueRequest", map[string]any{"requestId": p.RequestID}, nil)
	}()
}
