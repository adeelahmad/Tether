// Package app assembles a running Tether from a Config: Chrome, the session
// core, the faces and the server. cmd/tether and the integration tests
// both start Tether through it.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	"github.com/adeelahmad/tether/internal/api"
	"github.com/adeelahmad/tether/internal/cdp"
	"github.com/adeelahmad/tether/internal/certs"
	"github.com/adeelahmad/tether/internal/config"
	"github.com/adeelahmad/tether/internal/core"
	"github.com/adeelahmad/tether/internal/mcp"
	"github.com/adeelahmad/tether/internal/proxy"
	"github.com/adeelahmad/tether/internal/remote"
	"github.com/adeelahmad/tether/internal/server"
	"github.com/adeelahmad/tether/internal/session"
	"github.com/adeelahmad/tether/internal/webdriver"
)

// App is a started Tether.
type App struct {
	Addr     net.Addr
	CAPath   string
	Core     *core.Core
	Browser  string
	server   *server.Server
	listener net.Listener
	browser  *cdp.Chrome
	launched *cdp.Process
	log      *slog.Logger
}

// Start validates exposure, connects to (or launches) Chrome, and binds the
// listener. Call Serve to handle requests.
func Start(ctx context.Context, cfg *config.Config, log *slog.Logger) (_ *App, err error) {
	if err := server.CheckBind(ctx, cfg.Addr, cfg.Token, cfg.InsecureAllowPublic); err != nil {
		return nil, err
	}
	loopback, _ := server.IsLoopbackAddr(ctx, cfg.Addr)
	if !loopback && cfg.Token == "" {
		log.Warn("listening beyond loopback WITHOUT a token: anyone who can reach this port controls your browser")
	}

	a := &App{log: log}
	defer func() {
		if err != nil {
			a.cleanup()
		}
	}()

	wsURL := ""
	if cfg.Launch {
		a.launched, err = cdp.Launch(ctx, cdp.LaunchOptions{Bin: cfg.ChromeBin, UserDataDir: cfg.ProfileDir, Headless: cfg.Headless, ExtraArgs: cfg.ChromeArgs})
		if err != nil {
			return nil, fmt.Errorf("launching chrome: %w", err)
		}
		wsURL = a.launched.WSURL
		log.Info("launched chrome", "profile", cfg.ProfileDir, "headless", cfg.Headless)
	} else {
		wsURL, err = cdp.ResolveEndpoint(ctx, cfg.Chrome)
		if err != nil {
			return nil, err
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second) // Chrome may ask the user to allow the connection
	defer cancel()
	chrome, err := cdp.Connect(cctx, wsURL, log)
	if err != nil {
		return nil, err
	}
	a.browser = chrome
	chrome.SetLogBuffer(cfg.ConsoleBuffer)
	if a.Browser, err = chrome.Version(cctx); err != nil {
		return nil, fmt.Errorf("talking to chrome: %w", err)
	}

	sessions := session.NewManager(chrome, session.Options{IdleTimeout: cfg.IdleTimeout, Logger: log})
	a.Core = core.New(chrome, sessions, core.Options{
		Policy:  core.DomainAllowlist(cfg.AllowDomains),
		Logger:  log,
		Timeout: cfg.Timeout,
	})

	var ca *certs.Authority
	var caPEM []byte
	if !cfg.NoMITM {
		if ca, err = certs.LoadOrCreate(cfg.DataDir); err != nil {
			return nil, fmt.Errorf("loading local CA: %w", err)
		}
		caPEM = ca.CertPEM
		a.CAPath = certs.CertPath(cfg.DataDir)
	}

	px := proxy.New(a.Core, ca, log)
	wd := webdriver.New(a.Core, log)
	mcpFace := mcp.New(a.Core)
	ui := remote.New(a.Core)
	faces := []string{px.Name(), wd.Name(), mcpFace.Name() + " (planned)", ui.Name() + " (planned)"}
	a.server = server.New(server.Options{
		Token:     cfg.Token,
		Guard:     loopback && cfg.Token == "",
		Log:       log,
		Proxy:     px,
		API:       api.New(a.Core, caPEM, faces),
		WebDriver: wd,
		Faces:     map[string]core.Face{mcp.Prefix: mcpFace, remote.Prefix: ui},
	})

	var lc net.ListenConfig
	if a.listener, err = lc.Listen(ctx, "tcp", cfg.Addr); err != nil {
		return nil, err
	}
	a.Addr = a.listener.Addr()
	return a, nil
}

// Serve runs until ctx is cancelled or the browser connection drops, then
// shuts down gracefully: in-flight requests finish, Tether's own tabs are
// closed, and the browser connection is released.
func (a *App) Serve(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var lost atomic.Bool
	go func() {
		select {
		case <-a.browser.Done():
			lost.Store(true)
			a.log.Error("lost connection to chrome; shutting down")
			cancel()
		case <-ctx.Done():
		}
	}()
	go a.Core.Sessions.Run(ctx)

	err := a.server.Serve(ctx, a.listener, 10*time.Second)

	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	if !lost.Load() {
		a.Core.Sessions.Shutdown(sctx)
	}
	a.cleanup()
	if lost.Load() {
		return errors.New("lost connection to chrome")
	}
	return err
}

func (a *App) cleanup() {
	if a.listener != nil {
		_ = a.listener.Close()
	}
	if a.browser != nil {
		if a.launched != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = a.browser.Quit(ctx)
			cancel()
		}
		_ = a.browser.Close()
	}
	if a.launched != nil {
		_ = a.launched.Close()
	}
}
