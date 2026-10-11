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
| Token auth on every request when `--token` is set | Constant-time comparison; `Proxy-Authorization` for proxy traffic (stripped before forwarding), `Authorization` for the API/WebDriver. |
| Loopback guard when running without a token | Tether's own endpoints only answer a loopback `Host` with no cross-site `Origin`, so web pages in your browser cannot drive Tether via `fetch('http://127.0.0.1:8787')` or DNS rebinding. |
| Per-session `core.Policy` in the request path | `--allow-domains` restricts navigation for every face; it is the hook for future per-agent permissions. |
| Redacted logs | Attributes named like tokens, cookies, authorization or passwords are replaced with `[REDACTED]`; URLs are logged without query strings or userinfo. |
| Session cookie never forwarded | `tether_tab` is stripped before any request reaches the page or the site. Site `Set-Cookie` headers are not relayed to clients; the browser keeps its cookies. |
| Background tabs; adopted tabs never closed | Tether doesn't steal focus, and only closes tabs it opened. |
| Launched Chrome binds DevTools to `127.0.0.1` | The DevTools port itself is never exposed by Tether. |
| Remote UI login cookie is `HttpOnly`, `SameSite=Strict`, path-scoped and UI-only | Browsers can't send bearer tokens, so `/remote/login` trades the token for an HMAC-of-token cookie that scripts can't read, other sites can't trigger, and the proxy/API/WebDriver never accept. Rotating the token signs every browser out. |
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

Use Tailscale ACLs to limit which devices can reach port 8787. Avoid
`tailscale funnel` (that is public internet exposure).

## Threat model

**Assets.** The browser's authenticated sessions (cookies, storage, SSO), the
ability to act as the user on any site, and Tether's local CA private key
(`<data-dir>/ca-key.pem`), which can mint certificates for any host *that a
client trusts this CA for*.

**Trust boundary.** Whoever can authenticate to the Tether listener is fully
trusted to use the browser. Tether is not a multi-tenant isolation layer.

| Threat | Mitigation | Residual risk |
|---|---|---|
| Network attacker reaches the listener | Loopback default; token required off-loopback; Tailscale recommended | Running with `--insecure-allow-public` hands over your browser. |
| Malicious web page in the same browser calls Tether on localhost (CSRF, DNS rebinding) | Host + Origin + `Sec-Fetch-Site` guard | With `--token` set the guard is off, but browsers can't supply the token. |
| Token leaks via logs | Redacting log handler; secrets never logged | Tokens passed on the command line are visible in `ps`; prefer `TETHER_TOKEN` or `--token-file`. |
| Token leaks via `curl -x http://user:token@…` history | — | Shell history; prefer `--proxy-header "Proxy-Authorization: Bearer $TETHER_TOKEN"`. |
| Local CA key theft | Key written `0600` in the per-user config dir | Anyone with your user account already has your browser. Only trust the CA in clients that use Tether; don't add it to your OS trust store unless you understand the implication. |
| An agent/script navigates somewhere it shouldn't | `--allow-domains` policy enforced for every face | Allowlist is per-instance today; per-session/agent policies are the planned extension. |
| A site embeds or scripts the remote UI (clickjacking, cross-site websocket) | `frame-ancestors 'none'`, `X-Frame-Options: DENY`, Origin check on the websocket, `SameSite=Strict` cookie, Origin/`Sec-Fetch-Site` check on login | — |
| Remote UI token guessing | Login requires the full token (≥16 chars) and each failure is delayed | Use a long random token (`openssl rand -hex 32`). |
| Tether used to read a page the user has open | Only tabs Tether opened, or tabs explicitly addressed by ID, are driven | Any authenticated client can address any tab by ID. |
| Chrome DevTools port exposed | `--launch` binds it to loopback; Tether never re-exposes it | If you start Chrome yourself with `--remote-debugging-address=0.0.0.0`, that is outside Tether's control. |
| Denial of service (huge bodies, many tabs) | 32 MiB request body cap; per-tab serialisation; idle reaping | A token holder can open many tabs. |

**Out of scope.** Protecting the user from sites they are logged in to, from
other software running as the same OS user, or from someone with physical
access to an unlocked machine.
