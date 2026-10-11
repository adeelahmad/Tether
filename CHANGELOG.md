# Changelog

All notable changes to this project are documented here.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Tab-session core: one session per real Chrome tab, resolved by
  `X-Tether-Tab-ID` header → `tether_tab` cookie → one default tab per
  registrable domain (Public Suffix List). Idle timeout, explicit close, and
  a `410 tab_gone` error for dead tabs. Existing tabs can be adopted by ID.
- HTTP/HTTPS forward proxy. Requests are replayed inside the tab: `GET`/`HEAD`
  navigate and return the rendered DOM (or raw bytes with
  `X-Tether-Mode: raw`); other methods run as same-origin `fetch()` with the
  browser's cookies. HTTPS via a local CA (`/tether/v1/ca.pem`).
- W3C WebDriver endpoints for the common Selenium command set
  (see `docs/WEBDRIVER.md`), sharing the same sessions as the proxy.
- Control API under `/tether/v1/`: health, tab list/create/close, and
  JavaScript evaluation via `Runtime.evaluate`.
- Connects to an existing Chrome (`--chrome auto|port|host:port|ws://…|DevToolsActivePort`)
  or launches a dedicated one with a persistent profile (`--launch`).
- Security defaults: loopback-only bind, refusal to listen publicly without
  `--token` or `--insecure-allow-public`, bearer/basic token auth, a
  loopback Host/Origin guard against DNS rebinding, a per-session domain
  allowlist policy seam, and log redaction of tokens and cookies.
- Remote-browser web UI at `/remote/`, embedded in the binary: live video of
  any Chrome tab (CDP screencast, with screenshot keyframes for quiet and
  background tabs), mouse, wheel, touch, keyboard (IME and mobile keyboards
  included) and clipboard forwarding, JavaScript dialog relay, address bar,
  back/forward/reload, open/close tabs, watch-only mode and a phone layout.
  One screencast per tab is shared by all viewers. With `--token`, a sign-in
  page trades the token for an `HttpOnly`, `SameSite=Strict`, UI-only cookie.
  New flags `--no-remote` and `--remote-quality`. See `docs/REMOTE.md`.
- MCP face as a documented stub on the shared core.
- Sprint Zero: Makefile, golangci-lint, CI (lint → test + coverage →
  build matrix → govulncheck), CodeQL, Dependabot, GoReleaser (archives,
  checksums, multi-arch Docker image, Homebrew cask), community docs.

[Unreleased]: https://github.com/adeelahmad/tether/commits/main
