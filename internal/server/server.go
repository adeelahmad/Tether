// Package server wires Tether's faces onto one listener: the forward proxy
// (CONNECT and absolute-form requests), the control API, the WebDriver
// endpoints, the MCP face and the remote-UI seam. It owns authentication, the
// loopback guard, and graceful shutdown.
package server

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/adeelahmad/tether/internal/core"
	"github.com/adeelahmad/tether/internal/httpx"
	"github.com/adeelahmad/tether/internal/logx"
	"github.com/adeelahmad/tether/internal/proxy"
	"github.com/adeelahmad/tether/internal/version"
)

// Options configure a Server.
type Options struct {
	Token string
	// Guard enables the loopback Host/Origin guard on Tether's own
	// endpoints. Set it when listening on loopback without a token.
	Guard bool
	Log   *slog.Logger

	Proxy     http.Handler
	API       http.Handler
	WebDriver http.Handler
	Faces     map[string]core.Face // extra faces by mount prefix, e.g. "/mcp"
}

// Server is Tether's HTTP server.
type Server struct {
	handler http.Handler
	log     *slog.Logger
}

// New builds the handler chain.
func New(opts Options) *Server {
	lg := opts.Log
	if lg == nil {
		lg = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"name":    "tether",
			"version": version.Version,
			"docs":    "https://github.com/adeelahmad/tether#readme",
			"proxy":   "configure this address as your HTTP(S) proxy",
			"api":     "/tether/v1/",
		})
	})
	if opts.API != nil {
		mux.Handle("/tether/v1/", opts.API)
	}
	for prefix, f := range opts.Faces {
		mux.Handle(prefix, f)
		mux.Handle(prefix+"/", f)
	}
	if opts.WebDriver != nil {
		mux.Handle("/", opts.WebDriver)
	}

	var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if proxy.IsProxyRequest(r) {
			if opts.Proxy == nil {
				httpx.WriteError(w, http.StatusNotImplemented, core.CodeNotImplemented, "proxy disabled")
				return
			}
			opts.Proxy.ServeHTTP(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
	if opts.Guard {
		h = localGuard(h)
	}
	h = newAuthenticator(opts.Token).wrap(h)
	h = recoverer(lg, h)
	return &Server{handler: h, log: lg}
}

// Handler returns the full handler chain.
func (s *Server) Handler() http.Handler { return s.handler }

// Serve serves on ln until ctx is cancelled, then shuts down gracefully,
// giving in-flight requests up to grace to finish.
func (s *Server) Serve(ctx context.Context, ln net.Listener, grace time.Duration) error {
	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          log.New(&slogWriter{s.log}, "", 0),
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer cancel()
	err := srv.Shutdown(sctx)
	if errors.Is(err, context.DeadlineExceeded) {
		_ = srv.Close()
	}
	if serr := <-errc; serr != nil && !errors.Is(serr, http.ErrServerClosed) {
		return serr
	}
	return err
}

func recoverer(lg *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity
					panic(v)
				}
				lg.Error("panic serving request", "method", logx.Clean(r.Method), "path", logx.Clean(r.URL.Path), "panic", v)
				httpx.WriteError(w, http.StatusInternalServerError, core.CodeInternal, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type slogWriter struct{ l *slog.Logger }

func (w *slogWriter) Write(p []byte) (int, error) {
	w.l.Debug("http server", "msg", string(p))
	return len(p), nil
}
