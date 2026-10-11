# Changelog

All notable changes to this project are documented here.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- Relicensed from MIT to GPL-3.0-or-later.
- Connections are now accepted by a front door built on
  [snail007/goproxy](https://github.com/snail007/goproxy): its HTTP decoder
  classifies each connection (CONNECT, absolute-form proxy request, or a
  request for Tether's own endpoints) before `net/http` serves it.

### Added

- Docs site and landing page at <https://adeelahmad.github.io/Tether/>, built
  from `docs/` with MkDocs Material and deployed to GitHub Pages on every push
  to `main` (`make docs`, `make docs-serve`).
- Per-token and per-session domain policies (`docs/POLICIES.md`): a
  `--policy-file` defines named tokens, each with allow/deny rules, an
  `admin` flag and a `scripts` permission; `--deny-domains` adds a global
  denylist; tabs carry their own session rules
  (`POST /tether/v1/tabs {"policy": …}`, `GET`/`PUT /tether/v1/tabs/{id}/policy`,
  WebDriver `tether:options.policy`). Rules are layered (global ∩ token ∩
  owning token ∩ session) and restricted tabs are guarded in the browser via
  the DevTools `Fetch` domain, so redirects, link clicks and page script
  cannot leave the allowlist.
- Tab ownership: non-admin tokens only see and drive the tabs they opened,
  get their own per-site default tabs, and cannot adopt existing tabs.
- `GET /tether/v1/whoami` reports the calling token and its permissions.

- Per-tab console capture: `console.*` calls, uncaught exceptions and
  browser log entries (`Runtime.consoleAPICalled`, `Runtime.exceptionThrown`,
  `Log.entryAdded`) are kept in a ring buffer per tab (`--console-buffer`,
  default 1000, `0` disables) and exposed via
  `GET /tether/v1/tabs/{id}/logs?after=&level=`, WebDriver
  `get_log("browser")` (`/se/log`, `/log`), and `core.Core.Logs` for
  upcoming faces.
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
- WebDriver: frames (same- and cross-site, nested) and shadow roots (open
  and closed); alerts, confirms and prompts with `unhandledPromptBehavior`;
  multiple windows (New Window, popups, switching, closing); window rect,
  maximise, minimise and fullscreen for windows the session owns; file
  uploads, including Selenium's remote file upload; and Print Page.
- Background tabs get Chrome focus emulation, so real input reaches pages
  with cross-site iframes promptly.
- WebDriver Actions API (`POST`/`DELETE /session/{id}/actions`): key,
  mouse, pen, touch and wheel input as real CDP events, so `ActionChains`
  hover, drag and drop, double/context click, modifier chords and scrolling
  work.
- Control API under `/tether/v1/`: health, tab list/create/close, and
  JavaScript evaluation via `Runtime.evaluate`.
- Connects to an existing Chrome (`--chrome auto|port|host:port|ws://…|DevToolsActivePort`)
  or launches a dedicated one with a persistent profile (`--launch`).
- Security defaults: loopback-only bind, refusal to listen publicly without
  `--token` or `--insecure-allow-public`, bearer/basic token auth, a
  loopback Host/Origin guard against DNS rebinding, a per-session domain
  allowlist policy seam, and log redaction of tokens and cookies.
- MCP server face at `/mcp` (Streamable HTTP; MCP 2025-06-18, 2025-03-26 and
  2024-11-05) with 16 browser tools: tabs, navigation, accessibility-style
  snapshots with stable element refs, click, type, keys, select, scroll,
  wait, JavaScript, screenshots and in-tab `fetch()` with the user's cookies.
  Shares tabs, auth, the loopback guard and `--allow-domains` with the other
  faces; `--mcp=false` turns it off. See `docs/MCP.md`.
- `tether mcp`: a stdio bridge for MCP clients that don't speak HTTP
  (Claude Desktop, IDEs), local or across a tailnet.
- Screenshots of idle background tabs no longer hang: Tether brings the tab
  to the front and retries, as ChromeDriver does (WebDriver and MCP).
- Remote-browser web UI at `/remote/`, embedded in the binary: live video of
  any Chrome tab (CDP screencast, with screenshot keyframes for quiet and
  background tabs), mouse, wheel, touch, keyboard (IME and mobile keyboards
  included) and clipboard forwarding, JavaScript dialog relay, address bar,
  back/forward/reload, open/close tabs, watch-only mode and a phone layout.
  One screencast per tab is shared by all viewers. With `--token`, a sign-in
  page trades the token for an `HttpOnly`, `SameSite=Strict`, UI-only cookie.
  Each token signs in to its own UI session under its own rules and tabs.
  New flags `--no-remote` and `--remote-quality`. See `docs/REMOTE.md`.
- Sprint Zero: Makefile, golangci-lint, CI (lint → test + coverage →
  build matrix → govulncheck), CodeQL, Dependabot, GoReleaser (archives,
  checksums, multi-arch Docker image, Homebrew cask), community docs.

[Unreleased]: https://github.com/adeelahmad/tether/commits/main
