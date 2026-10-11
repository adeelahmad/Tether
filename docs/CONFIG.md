# Configuration file

Everything `tether serve` takes on the command line can also live in a TOML
file, along with a few things only the file can express: extra tokens,
browser headers, cookies to seed, and MCP OAuth clients.
[`tether.example.toml`](../tether.example.toml) is a complete, commented
starting point.

## Where Tether looks

1. `--config path/to/tether.toml`
2. `$TETHER_CONFIG`
3. `config.toml` in the per-user Tether directory, if it exists:
   - Linux: `~/.config/tether/config.toml` (or `$XDG_CONFIG_HOME/tether/`)
   - macOS: `~/Library/Application Support/tether/config.toml`
   - Windows: `%AppData%\tether\config.toml`

`--config=` (empty) skips the default file. A file named by `--config` or
`$TETHER_CONFIG` must exist.

## Precedence

```
command-line flag  >  TETHER_* environment variable  >  config file  >  built-in default
```

A setting given in a stronger layer replaces the weaker one entirely, lists
included: `--allow-domains github.com` replaces `[policy].allow`. The
exceptions are things that add up rather than override: tokens from
`--token`, `[auth]`, `[[auth.tokens]]` and `--policy-file` are all accepted
(duplicate names or secrets are an error), and `--policy-file`'s global rules
are merged into the others, as before.

Relative paths in the file are relative to the file; `~/` is your home
directory. Unknown keys are an error, so a typo never silently does nothing.

## Reference

| Key | Flag / env | Default | |
|---|---|---|---|
| **`[server]`** | | | |
| `addr` | `--addr` | `127.0.0.1:8787` | Listen address. Off-loopback needs a token. |
| `insecure_allow_public` | `--insecure-allow-public` | `false` | Listen off-loopback with no token (don't). |
| `public_url` | `--public-url` | from each request | Externally visible origin, e.g. `https://tether.tail1234.ts.net`. The OAuth issuer. |
| `timeout` | `--timeout` | `"60s"` | Default per-request timeout. |
| `idle_timeout` | `--idle-timeout` | `"15m"` | Close tabs Tether opened after inactivity; `"0s"` = never. |
| `data_dir` | `--data-dir` | user config dir | Local CA, launched profile, OAuth key. |
| `mitm` | `--no-mitm` (inverted) | `true` | `false` refuses HTTPS `CONNECT` instead of intercepting it. |
| **`[log]`** | | | |
| `level` | `--log-level` | `"info"` | `debug`, `info`, `warn`, `error`. |
| `format` | `--log-format` | `"text"` | `text` or `json`. |
| **`[auth]`** | | | |
| `token` / `token_env` / `token_file` | `--token` / — / `--token-file` | none | The admin token, at most one source. `token_env` names an environment variable to read it from. |
| `[[auth.tokens]]` | — | none | More tokens, each `name` plus one of `token`/`token_env`/`token_file`, and optional `admin`, `scripts`, `allow`, `deny`. Same shape as a [policy file](POLICIES.md) token. |
| **`[policy]`** | | | |
| `allow` | `--allow-domains` | all | Global navigation allowlist. |
| `deny` | `--deny-domains` | none | Global denylist; deny beats allow. |
| `file` | `--policy-file` | none | An extra JSON or TOML policy file, merged in. |
| **`[headers]`** | | | |
| `extra` | — | none | Table of headers Chrome sends with **every** request from the tabs Tether drives, to every site including third-party resources. Not for secrets. |
| `strip` | — | none | Client request headers the proxy drops before replaying a request in the browser. |
| **`[[cookies]]`** | — | none | Cookies stored in the browser's cookie jar at startup: `name`, `value` or `value_env`, `url` or `domain`, optional `path`, `secure`, `http_only`, `same_site` (`Strict`/`Lax`/`None`), `expires` (a TOML datetime; omit for a session cookie). |
| **`[chrome]`** | | | |
| `endpoint` | `--chrome` | `"auto"` | DevTools endpoint to connect to. |
| `launch` | `--launch` | `false` | Launch a dedicated Chrome instead. |
| `headless` | `--headless` | `false` | With `launch`. |
| `bin` | `--chrome-bin` | auto-detect | With `launch`. |
| `args` | `--chrome-args` | none | With `launch`. A list, so arguments may contain spaces. |
| `user_data_dir` | `--profile-dir` | `<data_dir>/chrome-profile` | With `launch`: Chrome's `--user-data-dir`. |
| `profile` | `--chrome-profile` | Chrome's choice | With `launch`: the profile inside it, e.g. `"Default"` or `"Profile 1"` (`--profile-directory`). |
| **`[console]`** | | | |
| `enabled` | — | `true` | `false` turns console capture off (same as `buffer = 0`). |
| `buffer` | `--console-buffer` | `1000` | Console and browser-log entries kept per tab. |
| **`[mcp]`** | | | |
| `enabled` | `--mcp` | `true` | Serve the MCP face at `/mcp`. |
| **`[mcp.oauth]`** | | | |
| `enabled` | `--mcp-oauth` | `false` | Let MCP clients sign in with OAuth. Needs a token. |
| `access_token_ttl` | — | `"1h"` | |
| `refresh_token_ttl` | — | `"720h"` | |
| `dynamic_registration` | — | `true` | Let clients register themselves (RFC 7591). |
| `[[mcp.oauth.clients]]` | — | none | Pre-registered clients: `id`, `name`, `redirect_uris`, and optionally `secret` or `secret_env` (≥16 characters) for a confidential client. |
| **`[remote]`** | | | |
| `enabled` | `--no-remote` (inverted) | `true` | The remote-browser UI at `/remote/`. |
| `quality` | `--remote-quality` | `70` | JPEG quality of its video, 1–100. |

## Choosing a Chrome profile

Chrome keeps profiles as directories inside one *user data directory*:
`Default`, `Profile 1`, `Profile 2`, … (`chrome://version` shows the one
you are in as "Profile Path"). With `launch = true`, `user_data_dir` picks the
data directory and `profile` picks the profile inside it:

```toml
[chrome]
launch = true
user_data_dir = "~/.config/tether/chrome-profile"
profile = "Profile 1"
```

Recent Chrome versions refuse remote debugging on your everyday data
directory, so point `user_data_dir` at a dedicated one and sign in there once.
When Tether connects to a Chrome you started yourself, it uses whatever
profile that window is running, and `profile` is ignored (with a warning).

## Headers and cookies

```toml
[headers]
extra = { "X-Purpose" = "automation" }   # Chrome sends this on every request
strip = ["X-Debug-Token"]               # never replay this client header

[[cookies]]
url = "https://example.com"
name = "consent"
value = "yes"
same_site = "Lax"
```

`extra` is applied to every tab Tether attaches to (new tabs and tabs it
adopts) with the DevTools `Network.setExtraHTTPHeaders` command. It goes to
every origin the page loads from, so it is for markers, not credentials.
`[[cookies]]` are written into the browser's cookie jar once, at startup,
exactly as if the site had set them; in a connected (not launched) Chrome that
is your real cookie jar.

## MCP OAuth

Some MCP clients (claude.ai custom connectors, Claude Desktop's remote
servers, several IDEs) can't send a fixed `Authorization` header; they only
speak the [MCP authorization spec](https://modelcontextprotocol.io/specification/2025-06-18/basic/authorization),
which is OAuth 2.1. With `[mcp.oauth] enabled = true` Tether is its own
authorization server:

```toml
[server]
addr = "100.101.102.103:8787"          # your tailnet address
public_url = "https://my-laptop.tail1234.ts.net"

[auth]
token_env = "TETHER_TOKEN"

[mcp.oauth]
enabled = true
```

1. The client calls `/mcp`, gets `401` with a `WWW-Authenticate` header
   pointing at `/.well-known/oauth-protected-resource/mcp`, and from there
   finds `/.well-known/oauth-authorization-server`.
2. It registers itself at `/oauth/register` (unless you turned dynamic
   registration off and gave it a pre-registered client).
3. It opens `/oauth/authorize` in your browser. The page asks for a **Tether
   token**; the client will act as that token, with its admin, script and
   domain permissions. Use a narrow `[[auth.tokens]]` entry for agents.
4. It exchanges the code (PKCE S256 is required) at `/oauth/token` and calls
   `/mcp` with the access token, refreshing as needed.

What to know:

- **OAuth tokens open `/mcp` only.** They are not accepted by the proxy,
  WebDriver, the control API or the remote UI.
- **Grants follow the token they were approved with.** Remove or change that
  token and every grant made with it stops working, access and refresh
  tokens alike.
- **Restarts are fine.** Registered clients and tokens are signed with a key
  in `<data_dir>/oauth.key` (created on first use, mode 0600); authorization
  codes live in memory for a minute. Delete the key file to revoke every
  client and grant at once.
- **`public_url`** should be set whenever Tether is reached through a proxy
  such as `tailscale serve`/`funnel`, so the issuer and metadata URLs are the
  ones the client sees. Without it, Tether uses the request's `Host` and
  `X-Forwarded-Proto`.
- Clients on the public internet (claude.ai) need to reach Tether over
  HTTPS. `tailscale funnel` does that, but it also puts the consent page and
  `/mcp` on the internet: use a long random token and narrow per-agent
  tokens.
