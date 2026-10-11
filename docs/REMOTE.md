# Remote browser UI

`tether serve` includes a web app at **`/remote/`**: open it in any browser —
your phone, another laptop on your tailnet, a tablet — pick a tab of the real
Chrome, and you see it live and can drive it with your mouse, keyboard and
clipboard. It is embedded in the binary; there is nothing else to install.

It is the fourth face on the shared core (see [ARCHITECTURE.md](ARCHITECTURE.md)),
so every tab is a regular Tether session: you can watch, or take over, the
very tab an agent, a proxied script or a Selenium test is driving, and hand it
back.

```sh
tether serve --launch
open http://127.0.0.1:8787/remote/     # or just http://127.0.0.1:8787/ in a browser
```

## What you get

| | |
|---|---|
| **Every tab** | The sidebar lists every page in Chrome, not only Tether's. Tabs Tether opened are marked *Tether*; your own tabs that a client has attached to are marked *In use*. Picking one of your own tabs attaches to it but never closes it. |
| **Live video** | Frames come from CDP `Page.startScreencast` as JPEG over a websocket; one stream per tab is shared by every viewer. |
| **Mouse** | Move, click, double/triple click, right and middle click, drag, and wheel/trackpad scrolling, mapped to the tab's CSS pixels whatever size the video is shown at. Touch screens tap and drag. |
| **Keyboard** | Real key events (so shortcuts, arrows, Tab, Enter and Escape behave), IME composition, and mobile on-screen keyboards (the ⌨ button). Ctrl/⌘ + A, C, X, Z, Y carry editing commands so they work on macOS Chrome too. |
| **Clipboard** | Paste (Ctrl/⌘+V) types your local clipboard into the tab. Copy (Ctrl/⌘+C) copies the tab's selection to your local clipboard. |
| **Dialogs** | `alert`, `confirm`, `prompt` and "leave this page?" are drawn by Chrome outside the page, so Tether relays them to the UI and sends your answer back. |
| **Navigation** | Address bar (URLs, bare hosts like `github.com`, or a search), back, forward, reload, new tab, close tab. Address-bar navigation goes through the core, so `--allow-domains` applies. |
| **Watch only** | The pointer toggle stops sending input, so you can watch an agent work without nudging it. |
| **Bring to front** | Activates the tab in Chrome. See *Smoothness* below. |
| **Full screen**, deep links (`/remote/#tab=<id>`), automatic reconnects, light and dark themes, phone layout. |

## Signing in

On loopback without a token (the default), there is nothing to sign in to:
the same Host/Origin guard that protects the rest of Tether applies.

With `--token`, browsers can't attach a bearer header to a page load, so the
UI has a small sign-in page at `/remote/login`. Enter the token once; Tether
answers with a cookie that is:

- **`HttpOnly`** and **`SameSite=Strict`** — scripts can't read it and other
  sites can't make your browser send it;
- **scoped to `/remote`** — it is accepted only for the UI, never for the
  proxy, the control API or WebDriver;
- **an HMAC of the token**, not the token — changing the token signs every
  browser out.

Password managers can store the token (the form is a normal username +
password form). *Sign out* clears the cookie. Scripts can still call the UI's
endpoints with `Authorization: Bearer <token>`.

## Over Tailscale

```sh
export TETHER_TOKEN="$(openssl rand -hex 32)"
tether serve --launch --addr "$(tailscale ip -4):8787"
# then browse to http://my-laptop:8787/remote/ from any device on your tailnet
```

For HTTPS with a real certificate (and a nicer URL), keep Tether on loopback
and let Tailscale terminate TLS for your tailnet only:

```sh
tether serve --launch --token "$TETHER_TOKEN"
tailscale serve --bg 8787        # https://my-laptop.<tailnet>.ts.net/remote/
```

Don't use `tailscale funnel`: that is the public internet.

## Smoothness

Chrome only paints a tab that is visible in its window. A **foreground** tab
streams at up to 60 fps. A **background** tab (Tether opens its tabs in the
background so it never steals your focus) is barely painted, so Tether fills
the gaps with screenshots: one per second while you watch, and four per
second for a few seconds after every click or key press, so typing still
feels immediate. Press **Bring to front** for full frame rate.

`--remote-quality` (1–100, default 70) trades JPEG quality for bandwidth.
Frames are capped at 1920×1080.

## Configuration

| Flag | Env | Default | |
|---|---|---|---|
| `--no-remote` | `TETHER_NO_REMOTE` | `false` | Don't serve the UI at all (`/remote/` is then a 404) |
| `--remote-quality` | `TETHER_REMOTE_QUALITY` | `70` | JPEG quality of the video |

## Security notes

- The UI is a full remote control of your logged-in browser. Treat its URL
  and token exactly like the rest of Tether ([SECURITY.md](../SECURITY.md)).
- Pages are served with a strict Content-Security-Policy (`script-src 'self'`,
  `frame-ancestors 'none'`), `X-Frame-Options: DENY` and
  `Referrer-Policy: no-referrer`, so the UI can't be framed or script-injected.
- The websocket checks that its `Origin` matches the host it was served from
  (or the `X-Forwarded-Host` a reverse proxy such as `tailscale serve` sets),
  so other sites can't open a socket to a tab.
- `--allow-domains` governs what the address bar and *New tab* can open. Like
  WebDriver clicks, links you click *inside* the page are the page's own
  navigation and are not re-checked.
- Watching a tab keeps it from being reaped as idle.

## Protocol

Small and documented so other clients can use it. One websocket per watched
tab: `GET /remote/ws/{tabId}`.

**Server → client**

- *Binary*: one frame = 4-byte big-endian length `N`, `N` bytes of JSON
  metadata (`deviceWidth`, `deviceHeight` in CSS pixels, `pageScaleFactor`,
  `scrollOffsetX/Y`, `offsetTop`, `timestamp`), then the JPEG.
- *Text*: JSON with a `t` field — `info` (`url`, `title`), `busy`
  (`busy: true|false` while a navigation runs), `error` (`code`, `message`),
  `clipboard` (`text`), `dialog` (`dialog`: `type`, `message`,
  `defaultPrompt`, `url`, or `closed: true`), `ended` (`code`: the tab was
  closed or Chrome went away; the socket then closes).

**Client → server** (text JSON, one per message)

| `t` | Fields | Effect |
|---|---|---|
| `mouse` | `type` (`mousePressed`, `mouseReleased`, `mouseMoved`), `x`, `y`, `button`, `buttons`, `clickCount`, `modifiers` | `Input.dispatchMouseEvent` |
| `wheel` | `x`, `y`, `deltaX`, `deltaY`, `modifiers` | `mouseWheel` |
| `key` | `type` (`keyDown`, `keyUp`, `rawKeyDown`, `char`), `key`, `code`, `keyCode`, `text`, `modifiers`, `commands` | `Input.dispatchKeyEvent`; a `keyDown` with no `text` is sent as `rawKeyDown` |
| `text` | `text` | `Input.insertText` (paste, IME) |
| `copy` | — | replies `clipboard` with the tab's selection |
| `dialog` | `accept`, `text` | answers the open JavaScript dialog |
| `nav` | `url` | navigate (policy applies) |
| `back`, `forward`, `reload`, `front` | — | history, reload, bring to front |

`x`/`y` are CSS pixels in the `deviceWidth × deviceHeight` viewport;
`modifiers` is Alt=1, Ctrl=2, Meta=4, Shift=8. Invalid messages get an
`error` reply and the stream continues.

REST helpers under `/remote/api/`: `GET tabs` (every Chrome page),
`POST tabs` (`{"url": "…"}` → a new tab), `DELETE tabs/{id}` (`?force=true`
to close a tab you didn't open through Tether), `GET info`.

## Why a websocket and not WebRTC

CDP hands out JPEG frames, not a video track. WebRTC would mean re-encoding
those frames to VP8/H.264 in the server, which needs either cgo codecs
(goodbye single static binary) or a pure-Go encoder too slow for real-time
video. WebRTC's other benefit, NAT traversal, is what Tailscale already
gives you. A websocket on the same listener inherits Tether's auth and
exposure rules for free, and the frame-dropping hub keeps latency low on slow
links (a viewer always gets the newest frame, never a backlog). The UI's
protocol is isolated in `internal/remote`, so a WebRTC transport can be added
beside it later without touching the core.

## Limitations

- File uploads by drag and drop aren't supported, and native popups Chrome
  draws outside the page (`<select>` lists, date pickers, permission
  prompts) don't appear in the video.
- One shared video stream per tab, sized by Chrome's viewport; the UI scales
  it to fit and does not resize your tab.
- Audio isn't streamed.
