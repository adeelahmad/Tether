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
	"syscall"

	"github.com/adeelahmad/tether/internal/app"
	"github.com/adeelahmad/tether/internal/certs"
	"github.com/adeelahmad/tether/internal/config"
	"github.com/adeelahmad/tether/internal/logx"
	"github.com/adeelahmad/tether/internal/version"
)

const usage = `tether — your logged-in Chrome, as a proxy, from anywhere. No extension.

Usage:
  tether serve [flags]   start the proxy + WebDriver server
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
		"auth", cfg.Token != "",
		"ca", a.CAPath,
	)
	fmt.Fprintf(stdout, "\n  Tether %s is serving %s on http://%s\n", version.Version, a.Browser, a.Addr)
	fmt.Fprintf(stdout, "  Proxy:     curl -x http://%s --cacert <(curl -s http://%s/tether/v1/ca.pem) https://example.com\n", a.Addr, a.Addr)
	fmt.Fprintf(stdout, "  WebDriver: http://%s  (Selenium remote URL)\n\n", a.Addr)

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
