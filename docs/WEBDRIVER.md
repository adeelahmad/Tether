# WebDriver support

Tether speaks the [W3C WebDriver](https://www.w3.org/TR/webdriver2/) wire
protocol for the commands most Selenium scripts use. The goal is "your script
just works against your real, logged-in browser", not full spec parity.
Endpoints are served at the root and under `/wd/hub` for older clients.

```python
from selenium import webdriver
from selenium.webdriver.common.by import By

driver = webdriver.Remote("http://127.0.0.1:8787", options=webdriver.ChromeOptions())
driver.get("https://github.com/notifications")      # already logged in
print(driver.find_element(By.CSS_SELECTOR, "h1").text)
driver.quit()                                         # closes only the tab Tether opened
```

With a token: `ClientConfig(remote_server_addr=..., auth_type=AuthType.BEARER, token=...)`
(recent Selenium 4 releases), or basic auth with any username and the token as password.

## Sessions are tabs

- `POST /session` opens a **new background tab**; the session ID *is* the
  tab ID, so you can also hit it through the proxy with
  `X-Tether-Tab-ID: <sessionId>`.
- To drive a tab that is already open, pass its ID:
  `{"capabilities": {"alwaysMatch": {"tether:options": {"tabId": "<id>"}}}}`.
  Attached tabs are left open when the session ends (override with
  `"keepTab": false`).
- Session rules: `{"tether:options": {"policy": {"allow": ["example.com"], "deny": ["admin.example.com"]}}}`
  restricts where the session's tab may navigate, on top of the global and
  token rules (see [POLICIES.md](POLICIES.md)). Navigating outside them fails
  with `unsupported operation`; *Execute Script* needs a token with the
  `scripts` permission.
- One session = one tab = one window handle. Window switching, new windows and
  frames are out of scope.

## Supported commands

| Command | Endpoint |
|---|---|
| Status | `GET /status` |
| New / Delete Session | `POST /session`, `DELETE /session/{id}` |
| Get / Set Timeouts | `GET`, `POST /session/{id}/timeouts` (script, pageLoad, implicit) |
| Navigate To / Get Current URL | `POST`, `GET /session/{id}/url` |
| Back / Forward / Refresh | `POST /session/{id}/back`, `/forward`, `/refresh` |
| Get Title | `GET /session/{id}/title` |
| Get Page Source | `GET /session/{id}/source` (live DOM) |
| Find Element(s) | `POST /session/{id}/element`, `/elements` |
| Find Element(s) From Element | `POST /session/{id}/element/{eid}/element`, `/elements` |
| Get Active Element | `GET /session/{id}/element/active` |
| Element Click | `POST /session/{id}/element/{eid}/click` |
| Element Clear | `POST /session/{id}/element/{eid}/clear` |
| Element Send Keys | `POST /session/{id}/element/{eid}/value` |
| Get Element Text / Tag Name / Rect | `GET …/element/{eid}/text`, `/name`, `/rect` |
| Get Element Attribute / Property / CSS Value | `GET …/element/{eid}/attribute/{n}`, `/property/{n}`, `/css/{n}` |
| Is Element Enabled / Selected / Displayed | `GET …/element/{eid}/enabled`, `/selected`, `/displayed` |
| Execute Script / Execute Async Script | `POST /session/{id}/execute/sync`, `/execute/async` |
| Get All / Named Cookie | `GET /session/{id}/cookie`, `/cookie/{name}` |
| Add Cookie | `POST /session/{id}/cookie` |
| Delete Cookie / All Cookies | `DELETE /session/{id}/cookie/{name}`, `/cookie` |
| Take Screenshot / Element Screenshot | `GET /session/{id}/screenshot`, `…/element/{eid}/screenshot` |
| Get Window Handle(s) | `GET /session/{id}/window`, `/window/handles` |
| Switch To Window (own handle only) | `POST /session/{id}/window` |
| Close Window (closes the tab, ends the session) | `DELETE /session/{id}/window` |
| Get Log / Get Log Types (Selenium extension) | `POST /session/{id}/se/log`, `GET …/se/log/types` (also `/log`, `/log/types`) — types `browser`, `driver` |
| Get / Set Window Rect, Maximize | `GET`/`POST …/window/rect`, `POST …/window/maximize` — reports the rect; resizing your real window is intentionally a no-op |

Locator strategies: `css selector`, `xpath`, `link text`,
`partial link text`, `tag name` (Selenium maps `By.ID`, `By.NAME` and
`By.CLASS_NAME` to CSS).

Behaviour notes:

- **Element references are stable**: they are Chrome backend node IDs, so
  finding the same node twice returns an equal element. After navigation
  they become `stale element reference`.
- **Click** scrolls the element into view, checks it is the topmost element
  at its centre (`element click intercepted` otherwise), dispatches a real
  mouse click, and waits for a navigation it triggers. Clicking an
  `<option>` selects it.
- **Send keys** focuses the element and types real key events; W3C special
  keys (Enter, Tab, arrows, Backspace, …) and sticky modifiers are supported.
  Enter waits for a form submission's navigation.
- **Page load strategy** `normal` (default), `eager` and `none` are honoured.
- **Browser logs** (`driver.get_log("browser")`) work like ChromeDriver:
  each call returns the console messages, uncaught exceptions and browser
  log entries since the previous call, with levels `DEBUG`, `INFO`,
  `WARNING` and `SEVERE`. Capture starts when Tether attaches to the tab and
  keeps the last `--console-buffer` entries; no `goog:loggingPrefs` needed.
  The `driver` log is always empty.
- **Cookies** are your browser's real cookies. `DELETE /cookie` deletes the
  current site's cookies from your profile — that *will* log you out.

## Not supported (yet)

Actions API (`/actions`), frames and shadow roots, alerts/prompts, new
window/tab creation, window resizing/fullscreen/minimise, file uploads,
print-to-PDF, and BiDi. These return `unknown command` (or
`unsupported operation`). Contributions welcome — they belong in
`internal/webdriver` on top of the existing `cdp.Tab` primitives.
