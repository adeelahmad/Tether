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
- To give the session a browser window of its own (so resizing it never
  touches yours), pass `"tether:options": {"newWindow": true}`.
- Every window handle is a tab ID. A session starts with one window and
  gains one for each **New Window** it opens and each **popup** its pages
  open (`window.open`, `target=_blank` links, popups of popups). Tabs that
  existed before the session started are never counted as its popups.
- Closing the last window ends the session. Deleting the session closes
  every window it owns: the tabs it opened and its popups, but not an
  attached tab (unless `keepTab` is false).

## Supported commands

| Command | Endpoint |
|---|---|
| Status | `GET /status` |
| New / Delete Session | `POST /session`, `DELETE /session/{id}` |
| Get / Set Timeouts | `GET`, `POST /session/{id}/timeouts` (script, pageLoad, implicit) |
| Navigate To / Get Current URL | `POST`, `GET /session/{id}/url` |
| Back / Forward / Refresh | `POST /session/{id}/back`, `/forward`, `/refresh` |
| Get Title | `GET /session/{id}/title` |
| Get Page Source | `GET /session/{id}/source` (live DOM of the current frame) |
| Get Window Handle / Handles | `GET /session/{id}/window`, `/window/handles` |
| Switch To Window | `POST /session/{id}/window` |
| New Window | `POST /session/{id}/window/new` (`"type": "tab"` or `"window"`) |
| Close Window | `DELETE /session/{id}/window` |
| Get / Set Window Rect | `GET`, `POST /session/{id}/window/rect` |
| Maximize / Minimize / Fullscreen Window | `POST /session/{id}/window/maximize`, `/minimize`, `/fullscreen` |
| Switch To Frame / Parent Frame | `POST /session/{id}/frame`, `/frame/parent` |
| Find Element(s) | `POST /session/{id}/element`, `/elements` |
| Find Element(s) From Element | `POST /session/{id}/element/{eid}/element`, `/elements` |
| Get Element Shadow Root | `GET /session/{id}/element/{eid}/shadow` |
| Find Element(s) From Shadow Root | `POST /session/{id}/shadow/{sid}/element`, `/elements` |
| Get Active Element | `GET /session/{id}/element/active` |
| Element Click | `POST /session/{id}/element/{eid}/click` |
| Element Clear | `POST /session/{id}/element/{eid}/clear` |
| Element Send Keys (including file inputs) | `POST /session/{id}/element/{eid}/value` |
| Get Element Text / Tag Name / Rect | `GET …/element/{eid}/text`, `/name`, `/rect` |
| Get Element Attribute / Property / CSS Value | `GET …/element/{eid}/attribute/{n}`, `/property/{n}`, `/css/{n}` |
| Is Element Enabled / Selected / Displayed | `GET …/element/{eid}/enabled`, `/selected`, `/displayed` |
| Execute Script / Execute Async Script | `POST /session/{id}/execute/sync`, `/execute/async` |
| Get All / Named Cookie | `GET /session/{id}/cookie`, `/cookie/{name}` |
| Add Cookie | `POST /session/{id}/cookie` |
| Delete Cookie / All Cookies | `DELETE /session/{id}/cookie/{name}`, `/cookie` |
| Dismiss / Accept Alert | `POST /session/{id}/alert/dismiss`, `/alert/accept` |
| Get / Send Alert Text | `GET`, `POST /session/{id}/alert/text` |
| Take Screenshot / Element Screenshot | `GET /session/{id}/screenshot`, `…/element/{eid}/screenshot` |
| Print Page | `POST /session/{id}/print` |
| Upload File (Selenium extension) | `POST /session/{id}/se/file` (also `/file`) |
| Perform / Release Actions | `POST`, `DELETE /session/{id}/actions` — key, pointer (mouse, pen, touch) and wheel input |
| Get Log / Get Log Types (Selenium extension) | `POST /session/{id}/se/log`, `GET …/se/log/types` (also `/log`, `/log/types`) — types `browser`, `driver` |

Locator strategies: `css selector`, `xpath`, `link text`,
`partial link text`, `tag name` (Selenium maps `By.ID`, `By.NAME` and
`By.CLASS_NAME` to CSS). From a shadow root, every strategy but `xpath`.

## Behaviour notes

- **Element references are stable**: they are Chrome backend node IDs, so
  finding the same node twice returns an equal element. After navigation
  they become `stale element reference`. Shadow roots get references the
  same way (`detached shadow root` once gone).
- **Click** scrolls the element (and the frames around it) into view,
  checks it is the topmost element at its centre (`element click
  intercepted` otherwise), dispatches a real mouse click, and waits for a
  navigation it triggers. Clicking an `<option>` selects it. Tether's tabs
  stay in the background; Chrome's focus emulation lets real input reach
  them, including cross-site iframes, without taking focus from you.
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
- **Cookies** are your browser's real cookies, for the current frame's
  document. `DELETE /cookie` deletes the current site's cookies from your
  profile — that *will* log you out.

### Frames

`Switch To Frame` takes `null` (the top-level page), a frame index (the
`n`th `<iframe>`/`<frame>` in the current document) or an `<iframe>`/`<frame>`
element. Same-site and **cross-site (out-of-process) frames** both work, at
any depth: finding, clicking, typing, scripts, page source, cookies and
element screenshots all run in the current frame. As in the spec, title,
URL and full screenshots stay top-level, and navigating (including back,
forward and refresh) returns to the top-level page. An element from another
frame answers `no such element`; a frame that disappears answers
`no such window` until you switch.

### Shadow DOM

`Get Element Shadow Root` returns open **and closed** shadow roots (DevTools
can see closed roots even though page scripts cannot); the browser's own
built-in roots (inside `<input>`, `<video>`, …) are not exposed. Shadow
roots can be passed to and returned from scripts.

### Alerts, confirms and prompts

JavaScript dialogs (`alert`, `confirm`, `prompt`, `beforeunload`) are
handled through the alert commands. A click, key press or navigation that
opens one returns normally; a script that opens one returns `null`.
`unhandledPromptBehavior` decides what happens when any other command runs
while a dialog is open: `dismiss and notify` (default), `accept and notify`,
`dismiss`, `accept` or `ignore`. The "notify" variants (and `ignore`)
answer `unexpected alert open` with the dialog text in `data.text`.

### Windows

New windows open in the background, like every Tether tab. **Window
geometry is only changed when the window holds nothing but this session's
tabs** (or other Tether tabs) — a window from `New Window` with
`"type": "window"`, or a session started with `"newWindow": true`. In a
window shared with tabs you are using, set rect, maximise, minimise and
fullscreen leave it alone and report its current rect, so scripts that call
`maximize_window()` keep working without rearranging your screen.

### File uploads

Sending keys to an `<input type="file">` sets its files: one path per line
(several only if the input has `multiple`). Paths are on the machine Tether
and Chrome run on, and must exist (`invalid argument` otherwise). Inputs
may be hidden, as styled file pickers usually are.

Remote Selenium clients use their file detector, which uploads the file to
`/session/{id}/se/file` and types the returned path; with Python,
`driver.file_detector = LocalFileDetector()`. Uploaded files live in a
temporary directory that is deleted when the session ends.

### Print to PDF

`Print Page` follows the spec's options (`orientation`, `scale`,
`background`, `page`, `margin` in centimetres, `pageRanges`) and returns a
base64 PDF of the current window's page. Chrome builds that can only print
when headless answer `unsupported operation` in a normal window.

## Actions

`ActionChains` and the W3C Actions API work: hover, drag and drop,
double and context click, modifier chords, held keys, wheel scrolling,
and multi-touch. Every action becomes a real CDP input event, so pages see
trusted `mousedown`/`keydown`/`wheel`/`touchstart` events, not synthetic
ones.

```python
from selenium.webdriver import ActionChains, Keys

ActionChains(driver).key_down(Keys.CONTROL).click(link).key_up(Keys.CONTROL).perform()
ActionChains(driver).drag_and_drop(card, column).perform()
ActionChains(driver).scroll_by_amount(0, 800).perform()
```

- **Sources**: `key`, `pointer` (`pointerType` `mouse`, `pen` or `touch`),
  `wheel` and `none`, with every action type the spec defines (`pause`,
  `keyDown`/`keyUp`, `pointerDown`/`pointerUp`/`pointerMove`/`pointerCancel`,
  `scroll`). Sources run tick by tick; a tick lasts as long as its longest
  `pause`, `pointerMove` or `scroll`, and moves and scrolls with a
  `duration` are spread over it frame by frame.
- **Origins**: `viewport`, `pointer` and element origins. An element's
  in-view centre point is used; unlike chromedriver, an element that is off
  screen is scrolled into view rather than failing. A target outside the
  viewport is `move target out of bounds`.
- **Keys**: all W3C key codes (modifiers left and right, F1–F12, numpad,
  navigation keys). Shift applies the US layout (`a`→`A`, `1`→`!`);
  Ctrl/Meta chords type no text. Modifiers held by a key source apply to
  pointer and wheel events in the same session.
- **Clicks**: a press within 500 ms and 4 px of the last one on the same
  button counts as a repeat click, so down/up/down/up is a double click.
  Pen pressure, tilt and twist, and touch width/height, are passed through.
- **State** (held keys and buttons, pointer positions) persists across
  `POST /actions` calls until `DELETE /actions`, which releases everything
  still held, newest source first.
- **Touch** brings the tab to the front, because Chrome only delivers touch
  input to a visible tab. Mouse, pen, keys and wheel work in a background
  tab. `pointerCancel` cancels touch contacts and is a no-op for mouse and
  pen.

## Not supported (yet)

Not planned for now: BiDi, `Get Computed Role / Label`, and the Selenium
Grid extensions other than file upload. These return `unknown command` (or
`unsupported operation`). Contributions welcome — they belong in
`internal/webdriver` on top of the existing `cdp.Tab` primitives.
