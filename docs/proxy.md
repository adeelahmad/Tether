# HTTP proxy & control API

Configure Tether as a plain HTTP/HTTPS forward proxy and any client gets your
real browser: `curl -x`, Python `requests`, another browser, anything. Each
request is replayed inside a real tab, with real cookies, storage and
JavaScript, and you get the rendered page back.

## How a request picks its tab

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

## Examples

```sh
P="-x http://127.0.0.1:8787 --cacert tether-ca.pem"

curl $P https://news.ycombinator.com/                     # rendered page, your session
curl $P -H 'X-Tether-Wait: networkidle' https://app.example.com/dashboard
curl $P -H 'X-Tether-Mode: raw' https://example.com/data.json
curl $P -X POST -H 'Content-Type: application/json' -d '{"q":1}' https://api.example.com/search
curl $P -i https://github.com/ | grep -i x-tether-tab-id  # which tab served it
```

## Request headers

| Request header | Meaning |
|---|---|
| `X-Tether-Tab-ID` | Use this tab (any open tab ID works; unknown → `410 tab_gone`) |
| `X-Tether-Mode` | `auto` (default), `rendered`, `raw`, `fetch` |
| `X-Tether-Wait` | `load` (default), `domcontentloaded`, `networkidle`, `none` |
| `X-Tether-Timeout` | Per-request timeout, e.g. `30s` |

Responses carry `X-Tether-Tab-ID`, `X-Tether-Final-URL` (after redirects)
and, on errors, a stable `X-Tether-Error` code (`tab_gone`, `forbidden`,
`navigation_failed`, `timeout`, …).

## Control API

```sh
curl -s http://127.0.0.1:8787/tether/v1/tabs                           # list sessions
curl -s -X POST http://127.0.0.1:8787/tether/v1/tabs -d '{"url":"https://example.com"}'
curl -s -X POST http://127.0.0.1:8787/tether/v1/tabs/$TAB/eval \
     -d '{"expression":"document.title"}'                               # Runtime.evaluate
curl -s "http://127.0.0.1:8787/tether/v1/tabs/$TAB/logs?level=warning" # console + browser logs
curl -s -X DELETE http://127.0.0.1:8787/tether/v1/tabs/$TAB            # close
```

## Console and browser logs

Every tab Tether drives keeps its recent `console.*` output, uncaught
exceptions and browser log entries (failed requests, security warnings).
`GET …/logs` returns `{"entries": [...], "next": N}`; pass `?after=N` to read
only what is new, and `?level=debug|info|warning|error` to filter. The same
buffer backs WebDriver's `driver.get_log("browser")`.
