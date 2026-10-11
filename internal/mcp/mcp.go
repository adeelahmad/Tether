// Package mcp is the seam for Tether's third face: a Model Context Protocol
// server that lets AI agents drive the real, logged-in browser over the
// tailnet with tools such as navigate, read_page, click, type, eval and
// screenshot.
//
// Status: stub. It is mounted at /mcp and answers 501 so clients get a
// clear signal, and it already holds the *core.Core it will build on.
// Every tool maps onto existing core operations (Sessions.Resolve/Create,
// Core.Do, Core.Eval, Core.Navigate) and is subject to core.Policy, which
// is where per-agent permissions will live. See docs/ARCHITECTURE.md.
package mcp

import (
	"net/http"

	"github.com/adeelahmad/tether/internal/core"
	"github.com/adeelahmad/tether/internal/httpx"
)

// Prefix is where the MCP face will be served (streamable HTTP transport).
const Prefix = "/mcp"

// Server is the MCP face.
type Server struct {
	core *core.Core
}

var _ core.Face = (*Server)(nil)

// New returns the MCP face over c.
func New(c *core.Core) *Server { return &Server{core: c} }

// Name implements core.Face.
func (s *Server) Name() string { return "mcp" }

// ServeHTTP implements core.Face. Not implemented yet.
func (s *Server) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteError(w, http.StatusNotImplemented, core.CodeNotImplemented,
		"the MCP face is planned; see docs/ARCHITECTURE.md")
}
