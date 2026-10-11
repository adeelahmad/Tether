package cdptest

import (
	"context"
	"errors"
	"time"

	"github.com/adeelahmad/tether/internal/cdp"
)

var _ cdp.Screen = (*Tab)(nil)

// FakeJPEG is the image every fake screencast frame carries.
var FakeJPEG = []byte("\xff\xd8\xff fake jpeg")

// FrameEvery is how often a fake screencast emits a frame.
var FrameEvery = 20 * time.Millisecond

// Screencast emits a FakeJPEG frame of an 800×600 viewport every
// FrameEvery until ctx ends or the tab is destroyed.
func (t *Tab) Screencast(ctx context.Context, _ cdp.ScreencastOptions, fn func(cdp.Frame)) error {
	t.mu.Lock()
	t.casts++
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.casts--
		t.mu.Unlock()
	}()
	tick := time.NewTicker(FrameEvery)
	defer tick.Stop()
	for n := 0; ; n++ {
		if !t.alive() {
			return cdp.ErrNoSuchTab
		}
		fn(cdp.Frame{Data: FakeJPEG, Meta: cdp.FrameMeta{DeviceWidth: 800, DeviceHeight: 600, PageScaleFactor: 1, Timestamp: float64(n)}})
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// Casting reports how many screencasts are running on the tab.
func (t *Tab) Casting() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.casts
}

func (t *Tab) alive() bool {
	t.b.mu.Lock()
	defer t.b.mu.Unlock()
	return t.b.tabs[t.id] == t
}

// Mouse records e.
func (t *Tab) Mouse(_ context.Context, e cdp.MouseEvent) error {
	if err := e.Validate(); err != nil {
		return err
	}
	t.mu.Lock()
	t.Mice = append(t.Mice, e)
	t.mu.Unlock()
	return nil
}

// InsertText records text.
func (t *Tab) InsertText(_ context.Context, text string) error {
	t.mu.Lock()
	t.Texts = append(t.Texts, text)
	t.mu.Unlock()
	return nil
}

// BringToFront counts activations.
func (t *Tab) BringToFront(context.Context) error {
	t.mu.Lock()
	t.Fronted++
	t.mu.Unlock()
	return nil
}

// WatchDialogs registers fn for OpenDialog.
func (t *Tab) WatchDialogs(fn func(cdp.Dialog)) (cancel func()) {
	t.mu.Lock()
	t.dialogFns = append(t.dialogFns, fn)
	i := len(t.dialogFns) - 1
	t.mu.Unlock()
	return func() {
		t.mu.Lock()
		t.dialogFns[i] = nil
		t.mu.Unlock()
	}
}

// OpenDialog simulates the page opening a JavaScript dialog.
func (t *Tab) OpenDialog(d cdp.Dialog) {
	t.mu.Lock()
	fns := append([]func(cdp.Dialog){}, t.dialogFns...)
	t.dialog = &d
	t.mu.Unlock()
	for _, fn := range fns {
		if fn != nil {
			fn(d)
		}
	}
}

// HandleDialog records the answer and closes the dialog.
func (t *Tab) HandleDialog(_ context.Context, accept bool, promptText string) error {
	t.mu.Lock()
	if t.dialog == nil {
		t.mu.Unlock()
		return errors.New("no dialog is showing")
	}
	t.dialog = nil
	t.DialogAnswers = append(t.DialogAnswers, DialogAnswer{accept, promptText})
	fns := append([]func(cdp.Dialog){}, t.dialogFns...)
	t.mu.Unlock()
	for _, fn := range fns {
		if fn != nil {
			fn(cdp.Dialog{Closed: true})
		}
	}
	return nil
}

// DialogAnswer is one HandleDialog call.
type DialogAnswer struct {
	Accept bool
	Text   string
}

// Answers returns the recorded dialog answers.
func (t *Tab) Answers() []DialogAnswer {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]DialogAnswer(nil), t.DialogAnswers...)
}

// Input returns copies of the recorded input.
func (t *Tab) Input() (keys []cdp.KeyEvent, mice []cdp.MouseEvent, texts []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]cdp.KeyEvent(nil), t.Keys...), append([]cdp.MouseEvent(nil), t.Mice...), append([]string(nil), t.Texts...)
}
