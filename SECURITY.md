# Security Policy

Tether exposes a **real, logged-in browser**. Anyone who can talk to a Tether
listener can act as you on every site that browser is signed in to. Treat a
Tether endpoint like an SSH key.

## Reporting a vulnerability

Please report vulnerabilities privately via
[GitHub Security Advisories](https://github.com/adeelahmad/tether/security/advisories/new).
Do not open public issues for security problems. We aim to acknowledge reports
within 72 hours and to ship a fix or mitigation for confirmed issues within
30 days. Only the latest release is supported with security fixes.

## Safe defaults (what Tether does for you)

| Default | Why |
|---|---|
| Binds to `127.0.0.1:8787` | Nothing off-machine can reach it unless you say so. |
| Refuses a non-loopback `--addr` without `--token` | Exposing a logged-in browser unauthenticated must be an explicit, loud choice (`--insecure-allow-public`). |
| Token auth on every request when `--token` or a policy file's tokens are set | Constant-time comparison against every token; `Proxy-Authorization` for proxy traffic (stripped before forwarding), `Authorization` for the API/WebDriver. |
| Per-token tab ownership | Non-admin tokens only see tabs they opened, get their own per-site default tabs, and cannot adopt the user's tabs. Another token's tab looks like a closed one. |
| Loopback guard when running without a token | Tether's own endpoints only answer a loopback `Host` with no cross-site `Origin`, so web pages in your browser cannot drive Tether via `fetch('http://127.0.0.1:8787')` or DNS rebinding. |
| Layered domain policy in the request path *and* in the browser | Global (`--allow-domains`/`--deny-domains`), per-token and per-tab rules ([docs/POLICIES.md](docs/POLICIES.md)). Restricted tabs get a DevTools `Fetch` guard, so redirects, link clicks and page script are held to the rules too. |
| Redacted logs | Attributes named like tokens, cookies, authorization or passwords are replaced with `[REDACTED]`; URLs are logged without query strings or userinfo. |
| Session cookie never forwarded | `tether_tab` is stripped before any request reaches the page or the site. Site `Set-Cookie` headers are not relayed to clients; the browser keeps its cookies. |
| Background tabs; adopted tabs never closed | Tether doesn't steal focus, and only closes tabs it opened. |
| Launched Chrome binds DevTools to `127.0.0.1` | The DevTools port itself is never exposed by Tether. |
| Remote UI login cookie is `HttpOnly`, `SameSite=Strict`, path-scoped and UI-only | Browsers can't send bearer tokens, so `/remote/login` trades the token for an HMAC-of-token cookie that scripts can't read, other sites can't trigger, and the proxy/API/WebDriver never accept. Rotating the token signs every browser out. |
| MCP OAuth is opt-in and scoped | With `[mcp.oauth]`, the consent page requires a Tether token (with a delay on wrong guesses, same-origin posts only, `frame-ancestors 'none'`), PKCE S256 is mandatory, redirect URIs must be https, loopback http, or an app scheme, and OAuth access tokens are accepted on `/mcp` only. Grants are bound to the approving token and die with it; deleting `oauth.key` revokes all. |
| Remote UI pages are locked down | Strict CSP (`script-src 'self'`, `frame-ancestors 'none'`), `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`; the websocket rejects foreign `Origin`s. `--no-remote` turns the UI off entirely. |

## Recommended exposure: Tailscale

To use Tether from another machine, **don't open a public port**. Put both
machines on a private [Tailscale](https://tailscale.com) tailnet and bind to
the Tailscale address with a token:

```sh
export TETHER_TOKEN="$(openssl rand -hex 32)"
tether serve --addr "$(tailscale ip -4):8787"
```

Then, from any machine on your tailnet:

```sh
curl -x "http://tether:$TETHER_TOKEN@my-laptop:8787" --cacert tether-ca.pem https://github.com/
```

Or, in the [config file](docs/CONFIG.md#tailscale), `[tailscale] enabled =
true` does the same, and `publish = "serve"` adds an HTTPS name on your
tailnet via `tailscale serve`.

Use Tailscale ACLs to limit which devices can reach port 8787. Avoid
`tailscale funnel` (`publish = "funnel"`) unless you need it: that is public
internet exposure, and Tether refuses either publish mode without a token.

## Threat model

**Assets.** The browser's authenticated sessions (cookies, storage, SSO), the
ability to act as the user on any site, and Tether's local CA private key
(`<data-dir>/ca-key.pem`), which can mint certificates for any host *that a
client trusts this CA for*.

**Trust boundary.** An admin token (including `--token`) is fully trusted to
use the browser. A non-admin token from a policy file is trusted only within
its domain rules and its own tabs; it is a guard rail for agents you run, not
a sandbox against a determined attacker who also shares the browser profile
(see *Limits* in [docs/POLICIES.md](docs/POLICIES.md)).

| Threat | Mitigation | Residual risk |
|---|---|---|
| Network attacker reaches the listener | Loopback default; token required off-loopback; Tailscale recommended | Running with `--insecure-allow-public` hands over your browser. |
| Malicious web page in the same browser calls Tether on localhost (CSRF, DNS rebinding) | Host + Origin + `Sec-Fetch-Site` guard | With `--token` set the guard is off, but browsers can't supply the token. |
| Token leaks via logs | Redacting log handler; secrets never logged | Tokens passed on the command line are visible in `ps`; prefer `TETHER_TOKEN` or `--token-file`. |
| Token leaks via `curl -x http://user:token@…` history | — | Shell history; prefer `--proxy-header "Proxy-Authorization: Bearer $TETHER_TOKEN"`. |
| Local CA key theft | Key written `0600` in the per-user config dir | Anyone with your user account already has your browser. Only trust the CA in clients that use Tether; don't add it to your OS trust store unless you understand the implication. |
| An agent/script navigates somewhere it shouldn't | Global, per-token and per-tab rules, checked in the core and enforced in the browser for top-level navigations (including redirects, clicks and script); MCP tools also check the page's current URL before reading or acting on it | Subresources and `fetch()` from page script are not restricted; give agents `"scripts": false`. |
| One agent reads or steers another agent's tab | Per-token ownership: non-admin tokens only resolve their own tabs, including via cookie, header and WebDriver session ID | Admin tokens see everything by design. |
| Tether used to read a page the user has open | Only tabs Tether opened, or tabs explicitly addressed by ID, are driven; adopting requires an admin token | Any admin token can address any tab by ID. |
| Prompt injection: a web page steers an AI agent connected over MCP | Server instructions tell the model page content is untrusted; tool annotations mark destructive tools; `--allow-domains` bounds where it can go; password values and `Set-Cookie` are never returned | An agent with access to your browser can be talked into acting as you. Use an allowlist and a client that confirms destructive tool calls. See [docs/MCP.md](docs/MCP.md#security). |
| A site embeds or scripts the remote UI (clickjacking, cross-site websocket) | `frame-ancestors 'none'`, `X-Frame-Options: DENY`, Origin check on the websocket, `SameSite=Strict` cookie, Origin/`Sec-Fetch-Site` check on login | — |
| Remote UI token guessing | Login requires the full token (≥16 chars) and each failure is delayed | Use a long random token (`openssl rand -hex 32`). |
| Chrome DevTools port exposed | `--launch` binds it to loopback; Tether never re-exposes it | If you start Chrome yourself with `--remote-debugging-address=0.0.0.0`, that is outside Tether's control. |
| A client attaches local files to a page (WebDriver file inputs) | Clients are fully trusted (see above); remote uploads land in a per-session temp dir deleted when the session ends | A token holder can make a page upload any file the Tether user can read. |
| Automation rearranges the person's screen | Window resize/maximise/minimise/fullscreen only act on windows holding nothing but Tether tabs | — |
| Secrets printed to the console by a page (tokens, PII in debug logs) | Console capture is only readable through the authenticated listener and is never written to Tether's own logs | Any token holder can read the captured console of any tab. `--console-buffer 0` turns capture off. |
| Sites detecting automation via `Runtime.enable` | — | Console capture enables the `Runtime` domain, which some anti-bot scripts can detect. `--console-buffer 0` leaves it off. |
| Denial of service (huge bodies, many tabs) | 32 MiB request body cap; per-tab serialisation; idle reaping; console buffer capped per tab and per entry (8 KiB) | A token holder can open many tabs. |

**Out of scope.** Protecting the user from sites they are logged in to, from
other software running as the same OS user, or from someone with physical
access to an unlocked machine.
