package webdriver

import (
	"context"
	"net/http"
)

// User prompts are the tab's JavaScript dialogs: alert, confirm, prompt
// and beforeunload.

func (h *Handler) alertText(_ context.Context, ws *wdSession, _ *http.Request, _ []byte) (any, error) {
	d := ws.s.Tab.Dialog()
	if d == nil {
		return nil, wdErr(ErrNoSuchAlert, "no user prompt is open")
	}
	return d.Message, nil
}

func (h *Handler) sendAlertText(_ context.Context, ws *wdSession, _ *http.Request, body []byte) (any, error) {
	var req struct {
		Text *string `json:"text"`
	}
	if err := decodeBody(body, &req); err != nil {
		return nil, err
	}
	if req.Text == nil {
		return nil, wdErr(ErrInvalidArgument, "text is required")
	}
	d := ws.s.Tab.Dialog()
	if d == nil {
		return nil, wdErr(ErrNoSuchAlert, "no user prompt is open")
	}
	if d.Type != "prompt" {
		return nil, wdErr(ErrElementNotInteractable, "a "+d.Type+" does not take text")
	}
	ws.promptText = req.Text
	return nil, nil
}

func (h *Handler) answerAlert(accept bool) command {
	return func(ctx context.Context, ws *wdSession, _ *http.Request, _ []byte) (any, error) {
		if ws.s.Tab.Dialog() == nil {
			return nil, wdErr(ErrNoSuchAlert, "no user prompt is open")
		}
		var text *string
		if accept {
			text = ws.promptText
		}
		if err := ws.s.Tab.HandleDialog(ctx, accept, text); err != nil {
			return nil, err
		}
		ws.promptText = nil
		return nil, nil
	}
}
