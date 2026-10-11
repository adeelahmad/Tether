// Command tether turns a real, logged-in Chrome into a network-accessible
// browser: an HTTP(S) proxy and WebDriver endpoint backed by real tabs,
// driven over the Chrome DevTools Protocol. No extension required.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/adeelahmad/tether/internal/app"
	"github.com/adeelahmad/tether/internal/certs"
	"github.com/adeelahmad/tether/internal/config"
	"github.com/adeelahmad/tether/internal/logx"
	"github.com/adeelahmad/tether/internal/mcp"
	"github.com/adeelahmad/tether/internal/version"
)

const usage = `tether — your logged-in Chrome, as a proxy, from anywhere. No extension.

Usage:
  tether serve [flags]   start the proxy, WebDriver and MCP server
  tether mcp [flags]     bridge a stdio MCP client (Claude Desktop, IDEs) to a running Tether
  tether ca [--data-dir]  print the local CA certificate path (trust it for HTTPS)
  tether version         print version information

Run 'tether serve -h' for serve flags. Docs: https://github.com/adeelahmad/tether
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "serve":
		return serve(args[1:], stdout, stderr)
	case "version", "--version", "-version", "-v":
		fmt.Fprintln(stdout, version.String())
		return 0
	case "ca":
		return ca(args[1:], stdout, stderr)
	case "mcp":
		return mcpBridge(args[1:], os.Stdin, stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprintf(stderr, "tether: unknown command %q\n\n%s", args[0], usage)
	return 2
}

func serve(args []string, stdout, stderr io.Writer) int {
	cfg, err := config.Parse(args, nil, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "tether:", err)
		return 2
	}
	log, err := logx.New(stderr, cfg.LogLevel, cfg.LogFormat)
	if err != nil {
		fmt.Fprintln(stderr, "tether:", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.Start(ctx, cfg, log)
	if err != nil {
		log.Error("startup failed", "err", err)
		return 1
	}
	log.Info("tether is ready",
		"version", version.Version,
		"addr", a.Addr.String(),
		"browser", a.Browser,
		"auth", cfg.HasAuth(),
		"ca", a.CAPath,
		"config", cfg.ConfigFile,
	)
	fmt.Fprintf(stdout, "\n  Tether %s is serving %s on http://%s\n", version.Version, a.Browser, a.Addr)
	fmt.Fprintf(stdout, "  Proxy:     curl -x http://%s --cacert <(curl -s http://%s/tether/v1/ca.pem) https://example.com\n", a.Addr, a.Addr)
	fmt.Fprintf(stdout, "  WebDriver: http://%s  (Selenium remote URL)\n", a.Addr)
	if cfg.MCP {
		fmt.Fprintf(stdout, "  MCP:       http://%s/mcp  (claude mcp add --transport http tether http://%s/mcp)\n", a.Addr, a.Addr)
	}
	if !cfg.NoRemote {
		fmt.Fprintf(stdout, "  Remote UI: http://%s/remote/  (watch and drive tabs from any browser)\n", a.Addr)
	}
	fmt.Fprintln(stdout)

	if err := a.Serve(ctx); err != nil {
		log.Error("server stopped", "err", err)
		return 1
	}
	log.Info("bye")
	return 0
}

func ca(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tether ca", flag.ContinueOnError)
	fs.SetOutput(stderr)
	def := config.DefaultDataDir()
	if v, ok := os.LookupEnv("TETHER_DATA_DIR"); ok {
		def = v
	}
	dir := fs.String("data-dir", def, "Tether data directory [$TETHER_DATA_DIR]")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if _, err := certs.LoadOrCreate(*dir); err != nil {
		fmt.Fprintln(stderr, "tether:", err)
		return 1
	}
	fmt.Fprintln(stdout, certs.CertPath(*dir))
	return 0
}

// mcpBridge runs `tether mcp`: stdio on one side, Tether's /mcp endpoint on
// the other. stdout carries protocol messages only; diagnostics go to stderr.
func mcpBridge(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tether mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	env := func(k, def string) string {
		if v, ok := os.LookupEnv(k); ok {
			return v
		}
		return def
	}
	url := fs.String("url", env("TETHER_MCP_URL", "http://"+config.DefaultAddr+mcp.Prefix), "Tether MCP endpoint [$TETHER_MCP_URL]")
	token := fs.String("token", env("TETHER_TOKEN", ""), "Tether bearer token [$TETHER_TOKEN]")
	tokenFile := fs.String("token-file", env("TETHER_TOKEN_FILE", ""), "read the bearer token from this file [$TETHER_TOKEN_FILE]")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *tokenFile != "" && *token == "" {
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			fmt.Fprintln(stderr, "tether:", err)
			return 2
		}
		*token = strings.TrimSpace(string(b))
	}
	log, _ := logx.New(stderr, "warn", "text")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := mcp.Bridge(ctx, mcp.BridgeOptions{URL: *url, Token: *token, Logger: log}, stdin, stdout); err != nil {
		fmt.Fprintln(stderr, "tether mcp:", err)
		return 1
	}
	return 0
}
