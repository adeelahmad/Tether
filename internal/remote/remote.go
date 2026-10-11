// Package remote is the seam for Tether's fourth face: an interactive
// remote-browser single-page app. It will stream a tab live (CDP
// Page.startScreencast, later WebRTC) and forward mouse and keyboard input
// back through the same cdp.Tab primitives WebDriver uses (Click, Key).
//
// Status: stub. It is mounted at /remote and answers 501. It consumes the
// same *core.Core as every other face, so a person can watch and take over
// the very tab an agent or script is driving. See docs/ARCHITECTURE.md.
package remote

import (
	"net/http"

	"github.com/adeelahmad/tether/internal/core"
	"github.com/adeelahmad/tether/internal/httpx"
)

// Prefix is where the remote UI will be served.
const Prefix = "/remote"

// UI is the remote-browser face.
type UI struct {
	core *core.Core
}

var _ core.Face = (*UI)(nil)

// New returns the remote UI face over c.
func New(c *core.Core) *UI { return &UI{core: c} }

// Name implements core.Face.
func (u *UI) Name() string { return "remote" }

// ServeHTTP implements core.Face. Not implemented yet.
func (u *UI) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteError(w, http.StatusNotImplemented, core.CodeNotImplemented,
		"the remote browser UI is planned; see docs/ARCHITECTURE.md")
}
