# Policies: per-token and per-session rules

Tether hands out a logged-in browser, so "who may take it where" matters.
Policies answer that in three layers, all enforced in the core — every face
(proxy, WebDriver, control API, and the MCP and remote-UI faces as they land)
is held to them.

| Layer | Set by | Scope |
|---|---|---|
| **Global** | `--allow-domains`, `--deny-domains`, the policy file's top-level `allow`/`deny` | Every request, every tab |
| **Token** | The policy file's `tokens[]` | Requests made with that token, *and* every tab it opens (the rules follow the tab) |
| **Session** | `POST /tether/v1/tabs` `{"policy": …}`, `PUT /tether/v1/tabs/{id}/policy`, WebDriver `tether:options.policy` | One tab |

A URL is allowed only if **every** layer allows it: global ∩ requesting
token ∩ owning token ∩ session. A lower layer can only narrow, and `deny`
always beats `allow`.

## Rule syntax

```json
{"allow": ["example.com", "*.github.io"], "deny": ["admin.example.com"]}
```

- `example.com` matches `example.com` and every subdomain.
- `*.example.com` matches subdomains only.
- `*` matches every host.
- An empty `allow` means "anything not denied".
- As soon as a layer has any rule, only `http`, `https`, `about:` and `data:`
  URLs pass it (`file:`, `chrome:` and `javascript:` do not).

## Tokens and the policy file

Give each agent or script its own token, so each gets its own rules, its own
tabs and its own name in the logs.

```sh
tether serve --policy-file examples/policy.json
```

```json
{
  "deny": ["accounts.google.com"],
  "tokens": [
    {"name": "research-agent", "token_env": "TETHER_RESEARCH_TOKEN",
     "allow": ["wikipedia.org", "arxiv.org"], "scripts": false},
    {"name": "mail-triage", "token_file": "mail-triage.token", "allow": ["mail.google.com"]},
    {"name": "ops", "token_env": "TETHER_OPS_TOKEN", "admin": true}
  ]
}
```

| Field | Default | |
|---|---|---|
| `name` | required | 1–64 of `A-Z a-z 0-9 . _ -`; shown in logs, tab listings and errors |
| `token` / `token_env` / `token_file` | exactly one | The secret (≥16 chars). `token_file` is relative to the policy file. Prefer `token_env` or `token_file` over inline secrets. |
| `allow`, `deny` | none | This token's rules |
| `admin` | `false` | See and drive every tab, including other tokens' tabs and tabs the user already had open (adoption) |
| `scripts` | `true` | Allow arbitrary JavaScript: `POST /tether/v1/tabs/{id}/eval` and WebDriver *Execute Script* |

`--token` still works and is the admin token named `default`. A policy file
with tokens enables authentication on its own, so it also satisfies the
"token required off loopback" rule. Unknown fields, duplicate names and
duplicate secrets are startup errors.

`GET /tether/v1/whoami` tells a client which token it is and what it may do.

## Tab ownership

- A tab belongs to the token that opened it. Non-admin tokens see and drive
  only their own tabs; another token's tab, or a tab the user opened, looks
  exactly like a tab that does not exist (`410 tab_gone`, WebDriver
  `invalid session id`), so tokens learn nothing about each other.
- The proxy's default tab per site is per token: two agents browsing
  `github.com` get two tabs.
- Only admin tokens can adopt tabs Tether did not open.
- With no authentication configured (loopback, no token) everything behaves
  as before: one implicit admin.

## Session rules

Narrow a single tab, for a task that should stay on one site:

```sh
# open a tab locked to one site
curl -s -X POST http://127.0.0.1:8787/tether/v1/tabs \
  -d '{"url":"https://github.com/me","policy":{"allow":["github.com"],"deny":["gist.github.com"]}}'

# inspect every layer that applies to a tab
curl -s http://127.0.0.1:8787/tether/v1/tabs/$TAB/policy

# replace the tab's session rules ({} clears them)
curl -s -X PUT http://127.0.0.1:8787/tether/v1/tabs/$TAB/policy -d '{"allow":["github.com"]}'
```

WebDriver:

```json
{"capabilities": {"alwaysMatch": {"tether:options": {"policy": {"allow": ["example.com"]}}}}}
```

Any token that can use a tab may change its session rules: they can never
grant more than the global and token rules already allow.

## Enforcement: in the core *and* in the browser

Checking the URL a client asks for is not enough: a page can redirect, a
WebDriver click can follow a link, and script can assign `location`. So for
every restricted tab (any layer has rules), Tether installs a browser-side
guard over the DevTools `Fetch` domain: every top-level document request the
tab makes is paused and allowed only if the tab's policy (global ∩ owning
token ∩ session) allows it; otherwise Chrome fails it with
`net::ERR_BLOCKED_BY_CLIENT` and the forbidden page never loads.
Unrestricted tabs, including the user's own tabs under an admin token, are
never intercepted.

As a second line, the core re-checks the URL a request finally landed on.
Requests that end somewhere forbidden return `403 forbidden`, and a
navigation that got there leaves the tab on `about:blank`.

## Limits

- Rules govern **navigation** (top-level documents). Subresources and
  third-party iframes on an allowed page load normally, and script with the
  `scripts` permission can still `fetch()` other origins. Set
  `"scripts": false` for tokens that should not run arbitrary JavaScript.
- Rules match host names, not paths or ports.
- Tokens are static; change the policy file and restart to rotate them.
