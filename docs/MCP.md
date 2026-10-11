# MCP: give your agent your real browser

Tether serves a [Model Context Protocol](https://modelcontextprotocol.io)
server at `/mcp` on the same listener as the proxy and WebDriver. Any MCP
client (Claude Code, Claude Desktop, Cursor, VS Code, your own agent) gets
tools to read and drive your real, logged-in Chrome: no extension, no cloud
browser, no re-login.

- **Transport:** Streamable HTTP (MCP `2025-06-18`, also `2025-03-26` and
  `2024-11-05` clients). Stdio-only clients use the `tether mcp` bridge.
- **Auth:** the same bearer token as everything else (`--token`), plus the
  loopback Host/Origin guard when running token-less on `127.0.0.1`.
- **Policy:** `--allow-domains` applies to every tool that loads or reads a
  page, exactly as it does for the proxy and WebDriver.
- **Turn it off:** `--mcp=false` (or `TETHER_MCP=false`).

## Connect a client

Start Tether as usual (`tether serve` or `tether serve --launch`).

**Claude Code**

```sh
claude mcp add --transport http tether http://127.0.0.1:8787/mcp
# with a token (e.g. across your tailnet):
claude mcp add --transport http tether http://my-laptop:8787/mcp \
  --header "Authorization: Bearer $TETHER_TOKEN"
```

**Claude Desktop, and other stdio clients** — add to the client's MCP config
(`claude_desktop_config.json` for Claude Desktop):

```json
{
  "mcpServers": {
    "tether": {
      "command": "tether",
      "args": ["mcp"],
      "env": { "TETHER_MCP_URL": "http://127.0.0.1:8787/mcp" }
    }
  }
}
```

`tether mcp` relays newline-delimited JSON-RPC on stdin/stdout to the HTTP
endpoint. It takes `--url` (`$TETHER_MCP_URL`) and `--token` / `--token-file`
(`$TETHER_TOKEN`, `$TETHER_TOKEN_FILE`), so the browser can live on another
machine on your tailnet while the agent runs here. When the client exits, the
bridge ends the MCP session and Tether closes the tabs that session opened.

**Cursor, VS Code, anything that speaks Streamable HTTP** — point it at
`http://127.0.0.1:8787/mcp` and, if you use a token, send
`Authorization: Bearer <token>`.

**Clients that sign in with OAuth** (claude.ai custom connectors, remote
servers in Claude Desktop, IDEs that can't send a fixed header) — enable
`[mcp.oauth]` in the [config file](CONFIG.md#mcp-oauth) (or pass
`--mcp-oauth`), add `http(s)://<tether>/mcp` in the client, and approve the
sign-in page with a Tether token. The client then acts as that token.

## How a session works

Each MCP session works in one **current tab**:

- `browser_navigate` opens a fresh background tab on first use (or use
  `browser_new_tab`).
- `browser_tabs` lists every page in Chrome, including tabs you opened
  yourself; `browser_select_tab` makes one of them current, which is how you
  hand an agent "the tab I'm looking at".
- Tabs are the same sessions the proxy and WebDriver use: a tab ID from
  `X-Tether-Tab-ID` or a WebDriver session works in `browser_select_tab`.
- When the MCP session ends (client disconnects, `DELETE /mcp`, or it sits
  idle for `--idle-timeout`), the tabs it **opened** are closed. Tabs it only
  selected are left open, always.

`browser_snapshot` is how an agent reads a page. It returns the title, URL,
scroll position, the visible interactive elements with refs, and the page
text:

```text
Page: Notifications
URL: https://github.com/notifications
Scroll: 0px of 3120px (viewport 900px)

Interactive elements:
[e212] link "Inbox" -> /notifications
[e230] searchbox "Search notifications" placeholder="Search notifications"
[e241] button "Mark as done" disabled
[e260] checkbox "Select all" checked=false
…

Text:
Notifications
Inbox 12 …
```

Refs (`e212`) are Chrome's backend node IDs, so the same element keeps the
same ref while the page lives and a ref from an old page fails as
`stale_element` rather than hitting the wrong element. They are the same
references the WebDriver face hands out.

## Tools

| Tool | What it does | Hints |
|---|---|---|
| `browser_tabs` | List open tabs; marks current, yours, and the user's own | read-only |
| `browser_new_tab` | Open a tab (optionally at `url`) and make it current | |
| `browser_select_tab` | Make an existing tab (`tab_id`) current | |
| `browser_close_tab` | Close a tab Tether opened; the user's tabs are only released | destructive |
| `browser_navigate` | Load `url` (`wait`: `load`, `domcontentloaded`, `networkidle`, `none`) | |
| `browser_history` | `back`, `forward` or `reload` | |
| `browser_snapshot` | Title, URL, interactive elements with refs, page text (`max_elements`, `max_text`) | read-only |
| `browser_click` | Real mouse click on `ref` or `selector`; waits for any navigation; `force` uses `element.click()` | destructive |
| `browser_type` | Focus and type `text` with real key events; `clear` (default true), `submit` presses Enter | destructive |
| `browser_press_key` | A key or chord: `Enter`, `Escape`, `ArrowDown`, `Control+A`, `Shift+Tab` | destructive |
| `browser_select_option` | Pick `values` (by value or label) in a `<select>` | destructive |
| `browser_scroll` | Scroll by a page or `pixels` in a `direction`, or scroll `ref` into view | |
| `browser_wait_for` | Wait for `text` and/or a visible `selector` (or `gone`), up to `timeout_ms` | read-only |
| `browser_eval` | Evaluate a JavaScript expression in the page (`Runtime.evaluate`), promises awaited | destructive |
| `browser_screenshot` | PNG of the viewport, `full_page`, or one `ref` | read-only |
| `browser_fetch` | HTTP request from inside the tab with the user's cookies (`method`, `headers`, `body`) | destructive |

Every tool advertises MCP annotations (`readOnlyHint`, `destructiveHint`,
`openWorldHint`) so clients can decide what to confirm with you.

Failures come back as tool results with `isError: true` and a stable code
the model can act on: `tab_gone`, `forbidden`, `stale_element`,
`no_such_element`, `element_not_interactable`, `no_current_tab`,
`invalid_arguments`, `timeout`, `navigation_failed`, `javascript_error`.

### Notes and limits

- Snapshots cover the main frame. Elements inside iframes and closed shadow
  roots are not listed (reach them with `browser_eval`).
- Password field values are never read back (`value="(hidden)"`), and
  `browser_fetch` never returns `Set-Cookie`. Tool arguments are never
  logged, since they can hold what an agent typed into a form.
- `browser_fetch` runs `fetch()` in the current tab; if the tab is on a
  different origin it is first moved to the target origin's root, the same
  as the proxy's fetch mode.
- Screenshots of an idle background tab briefly bring that tab to the front
  (Chrome does not paint hidden tabs), as ChromeDriver does.
- Resource and prompt endpoints, sampling and server-initiated streams are
  not offered; `GET /mcp` answers `405`.

## Security

An MCP client connected to Tether can act as you on every site your browser
is signed in to. In addition to [SECURITY.md](../SECURITY.md):

- **Prompt injection is the main risk.** Web pages are untrusted input, and a
  page can contain text written to steer an agent. Tether tells the model so
  in its server instructions, but the real controls are yours: run with
  `--allow-domains` for the sites the agent needs, and use a client that asks
  before calling destructive tools.
- **Policy is enforced against the page itself.** Every tool that reads or
  acts on a page first checks the page's current URL against
  `--allow-domains`, so an agent that clicked its way off the allowlist can't
  read or act on where it landed.
- **Tokens:** prefer `TETHER_TOKEN` or `--token-file` over `--token` so the
  token does not show up in `ps` or in an MCP config file.
- **OAuth** grants act as the token approved on the consent page, open `/mcp`
  only, and die when that token is removed or changed. Approve agents with a
  narrow per-agent token rather than the admin one.

## Protocol details

| | |
|---|---|
| Endpoint | `POST /mcp` (JSON-RPC message or batch), `DELETE /mcp` (end session) |
| Session | `Mcp-Session-Id` header, issued on `initialize`; `404` once it ends |
| Version header | `Mcp-Protocol-Version` validated when present |
| Responses | `application/json`; `202` for notifications and responses |
| Capabilities | `tools` (`listChanged: false`) |
| Limits | 4 MiB per message; 64 concurrent MCP sessions (least recently used is ended) |
