<div align="center">

# Tether

### Your logged-in Chrome — as a proxy, from anywhere. No extension.

**Give your AI agent (or your script, or your other laptop) your real, logged-in browser.**<br/>
One self-contained binary. Speaks plain HTTP proxy and WebDriver. Drives Chrome over the DevTools Protocol.

[![CI](https://github.com/adeelahmad/tether/actions/workflows/ci.yml/badge.svg)](https://github.com/adeelahmad/tether/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/adeelahmad/tether)](https://goreportcard.com/report/github.com/adeelahmad/tether)
[![Latest release](https://img.shields.io/github/v/release/adeelahmad/tether?sort=semver)](https://github.com/adeelahmad/tether/releases)
[![License: MIT](https://img.shields.io/github/license/adeelahmad/tether)](LICENSE)

<img src="docs/demo.gif" alt="Tether driving a real Chrome tab: curl through the proxy returns a logged-in page" width="760"/>

</div>

---

Agents and scripts keep hitting the same wall: the page they need is behind
a login, an SSO dance, a captcha you already solved, or a cookie banner you
already dismissed — **in your browser**. Every tool that bridges into that
browser today asks you to install an extension.

**Tether doesn't.** It attaches to Chrome over the DevTools Protocol that
Chrome already speaks, and turns each tab into something any client can use:

- 🌐 **An HTTP/HTTPS proxy.** `curl -x`, Python `requests`, another browser,
  anything. Each request is replayed *inside a real tab*: real cookies, real
  storage, real JavaScript. You get the rendered page back.
- 🤖 **A WebDriver endpoint.** Point Selenium at it and most scripts just
  work — against your actual signed-in profile.
- 🧩 **One core, four faces.** The proxy and WebDriver ship today; an MCP
  server for agents and a live remote-browser UI are designed in and
  stubbed ([architecture](docs/ARCHITECTURE.md)).
- 🔒 **Safe by default.** Loopback-only unless you set a token. Built to sit
  on a private [Tailscale](https://tailscale.com) tailnet, not the internet.

## 30-second quickstart

**1. Install**

```sh
go install github.com/adeelahmad/tether/cmd/tether@latest
```
<sub>Or `brew install --cask adeelahmad/tap/tether`, or grab a binary from [Releases](https://github.com/adeelahmad/tether/releases).</sub>

**2. Start** — this opens a dedicated Chrome window. Log in to a site (say, GitHub) once; the profile persists.

```sh
tether serve --launch
```

**3. Prove it** — in another terminal, fetch a logged-in page through the proxy:

```sh
curl -s http://127.0.0.1:8787/tether/v1/ca.pem -o tether-ca.pem
curl -s -x http://127.0.0.1:8787 --cacert tether-ca.pem https://github.com/settings/profile \
  | grep -o '<title>[^<]*</title>'
```

You get your profile settings page — the one that redirects to a login
screen for everyone else. 🎉

> **Prefer your everyday Chrome?** Skip `--launch`. On Chrome 144+, open
> `chrome://inspect/#remote-debugging`, allow remote debugging, and run
> `tether serve` — it finds the browser automatically. On any Chrome, you can
> also start it with `--remote-debugging-port=9222 --user-data-dir=<dir>`
> (Chrome requires a non-default profile directory for the debugging port).

## How it works

```mermaid
flowchart LR
  A["curl / requests / agent"] -- "HTTP proxy" --> T
  B["Selenium"] -- "WebDriver" --> T
  T["tether serve<br/>sessions · policy · auth"] -- "DevTools Protocol" --> C
  subgraph C["Your Chrome (logged in)"]
    T1["tab: github.com"]
    T2["tab: mail.google.com"]
    T3["tab: WebDriver session"]
  end
```

**One session = one real tab.** Tether picks the tab for each request by:

1. an explicit `X-Tether-Tab-ID` header, else
2. the `tether_tab` cookie Tether set on first contact, else
3. **one default tab per site** (registrable domain via the Public Suffix
   List, so `shop.example.co.uk` and `www.example.co.uk` share a tab).

On first contact Tether opens a fresh background tab and returns its ID as
both an `X-Tether-Tab-ID` header and a `Set-Cookie`. That cookie is never
forwarded to Chrome or the site. A WebDriver session ID *is* a tab ID, so the
proxy and WebDriver can drive the same tab.

`GET`/`HEAD` navigate the tab and return the rendered DOM (or the original
bytes with `X-Tether-Mode: raw`). `POST` and friends run as a same-origin
`fetch()` inside the tab, so the browser attaches its own cookies. HTTPS works
by terminating `CONNECT` with a local CA you trust once (`tether ca` prints
its path).

## Use it

### Proxy

```sh
P="-x http://127.0.0.1:8787 --cacert tether-ca.pem"

curl $P https://news.ycombinator.com/                     # rendered page, your session
curl $P -H 'X-Tether-Wait: networkidle' https://app.example.com/dashboard
curl $P -H 'X-Tether-Mode: raw' https://example.com/data.json
curl $P -X POST -H 'Content-Type: application/json' -d '{"q":1}' https://api.example.com/search
curl $P -i https://github.com/ | grep -i x-tether-tab-id  # which tab served it
```

| Request header | Meaning |
|---|---|
| `X-Tether-Tab-ID` | Use this tab (any open tab ID works; unknown → `410 tab_gone`) |
| `X-Tether-Mode` | `auto` (default), `rendered`, `raw`, `fetch` |
| `X-Tether-Wait` | `load` (default), `domcontentloaded`, `networkidle`, `none` |
| `X-Tether-Timeout` | Per-request timeout, e.g. `30s` |

Responses carry `X-Tether-Tab-ID`, `X-Tether-Final-URL` (after redirects)
and, on errors, a stable `X-Tether-Error` code (`tab_gone`, `forbidden`,
`navigation_failed`, `timeout`, …).

### WebDriver

```python
from selenium import webdriver
d = webdriver.Remote("http://127.0.0.1:8787", options=webdriver.ChromeOptions())
d.get("https://github.com/notifications")
print(d.title)
d.quit()
```

Covered commands and behaviour: [docs/WEBDRIVER.md](docs/WEBDRIVER.md).

### Control API

```sh
curl -s http://127.0.0.1:8787/tether/v1/tabs                           # list sessions
curl -s -X POST http://127.0.0.1:8787/tether/v1/tabs -d '{"url":"https://example.com"}'
curl -s -X POST http://127.0.0.1:8787/tether/v1/tabs/$TAB/eval \
     -d '{"expression":"document.title"}'                               # Runtime.evaluate
curl -s -X DELETE http://127.0.0.1:8787/tether/v1/tabs/$TAB            # close
```

### From another machine (Tailscale)

```sh
export TETHER_TOKEN="$(openssl rand -hex 32)"
tether serve --launch --addr "$(tailscale ip -4):8787"
# elsewhere on the tailnet:
curl -x "http://tether:$TETHER_TOKEN@my-laptop:8787" --cacert tether-ca.pem https://github.com/
```

Tether refuses to listen beyond loopback without a token. Read
[SECURITY.md](SECURITY.md) before exposing it anywhere.

## Configuration

Every flag has a `TETHER_*` environment variable; flags win.

| Flag | Default | |
|---|---|---|
| `--addr` | `127.0.0.1:8787` | Listen address |
| `--token` / `--token-file` | — | Bearer token (≥16 chars); required off-loopback |
| `--insecure-allow-public` | `false` | Allow off-loopback with no token (don't) |
| `--chrome` | `auto` | `auto`, a port, `host:port`, `http://…`, `ws://…`, or a `DevToolsActivePort` path |
| `--launch` / `--headless` | `false` | Launch a dedicated Chrome (persistent profile under `--data-dir`) |
| `--chrome-bin`, `--chrome-args`, `--profile-dir` | auto | With `--launch` |
| `--idle-timeout` | `15m` | Close tabs Tether opened after inactivity (`0` = never) |
| `--timeout` | `60s` | Default per-request timeout |
| `--allow-domains` | all | Comma-separated navigation allowlist (applies to every face) |
| `--data-dir` | user config dir | Local CA and launched profile |
| `--no-mitm` | `false` | Refuse HTTPS `CONNECT` instead of intercepting |
| `--log-level`, `--log-format` | `info`, `text` | `json` for machines; secrets are always redacted |

Docker (headless Chromium inside the image):

```sh
docker run --rm -p 8787:8787 -e TETHER_TOKEN=$(openssl rand -hex 32) -v tether:/data ghcr.io/adeelahmad/tether
```

## How Tether compares

An honest look. Tether's trade-off is that HTTPS proxying needs you to trust a
local CA, and it only drives Chromium-family browsers.

| | **Tether** | Extension-based bridges | Cloud browsers | Fresh headless automation |
|---|---|---|---|---|
| Uses your real logged-in profile | ✅ | ✅ | ❌ (re-login, stored creds) | ❌ |
| Needs a browser extension | **No** | Yes | No | No |
| Plain HTTP proxy (curl, any language) | ✅ | ❌ | Rarely | ❌ |
| W3C WebDriver / Selenium | ✅ incl. frames, alerts, windows, uploads, PDF | ❌ | Often | ✅ |
| MCP for agents | Planned | Often | Often | Via wrappers |
| Runs entirely on your machine | ✅ | ✅ | ❌ | ✅ |
| Remote access | Tailscale + token | Usually local only | ✅ | DIY |
| Single static binary | ✅ | ❌ | n/a | ❌ (driver + runtime) |
| Browsers | Chrome / Chromium / Edge | Varies | Varies | Many |

## Status & roadmap

- [x] Tab-session core, HTTP/HTTPS proxy, WebDriver common set, control API
- [x] Safe-by-default exposure, token auth, domain allowlist seam
- [ ] MCP server face (tools over the same core) — stubbed at `/mcp`
- [ ] Remote-browser UI with live screencast and input — stubbed at `/remote`
- [ ] Per-session / per-agent policies; console log capture; WebDriver Actions

## Contributing

Issues and PRs welcome — start with [CONTRIBUTING.md](CONTRIBUTING.md) and
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md). `make check` runs what CI runs.

## License

[MIT](LICENSE) © Adeel Ahmad
