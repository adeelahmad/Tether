# Configuration

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
| `--console-buffer` | `1000` | Console/log entries kept per tab (`0` = no capture) |
| `--allow-domains` | all | Comma-separated navigation allowlist (applies to every face) |
| `--deny-domains` | none | Comma-separated denylist; deny beats allow |
| `--policy-file` | — | JSON file with per-token rules and tokens, see [Policies](POLICIES.md) |
| `--data-dir` | user config dir | Local CA and launched profile |
| `--no-mitm` | `false` | Refuse HTTPS `CONNECT` instead of intercepting |
| `--mcp` | `true` | Serve the MCP face at `/mcp` |
| `--no-remote` | `false` | Don't serve the remote-browser UI at `/remote/` |
| `--remote-quality` | `70` | JPEG quality of the remote UI video (1–100) |
| `--log-level`, `--log-format` | `info`, `text` | `json` for machines; secrets are always redacted |

## From another machine (Tailscale)

```sh
export TETHER_TOKEN="$(openssl rand -hex 32)"
tether serve --launch --addr "$(tailscale ip -4):8787"
# elsewhere on the tailnet:
curl -x "http://tether:$TETHER_TOKEN@my-laptop:8787" --cacert tether-ca.pem https://github.com/
# or watch and drive it from your phone: http://my-laptop:8787/remote/
```

Tether refuses to listen beyond loopback without a token. Read
[Security](security.md) before exposing it anywhere.

## One token per agent

Give every agent its own token with its own domain rules. Each token only
sees the tabs it opened, and the rules are enforced inside the browser, so
redirects, link clicks and page script can't wander off the allowlist either.

```sh
tether serve --policy-file examples/policy.json   # tokens + rules, see Policies
curl -s -H "Authorization: Bearer $TETHER_RESEARCH_TOKEN" http://127.0.0.1:8787/tether/v1/whoami
# lock a single tab tighter still
curl -s -H "Authorization: Bearer $TETHER_RESEARCH_TOKEN" -X POST http://127.0.0.1:8787/tether/v1/tabs \
  -d '{"url":"https://en.wikipedia.org/","policy":{"allow":["en.wikipedia.org"]}}'
```
