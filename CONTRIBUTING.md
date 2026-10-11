# Contributing to Tether

Thanks for helping! Tether hands out access to a real, logged-in browser, so
we care about two things above all: **it must be safe by default**, and **the
core must stay small enough to reason about**.

## Getting started

```sh
git clone https://github.com/adeelahmad/tether && cd tether
make build            # ./bin/tether
make test             # unit tests, race detector on
make test-integration # end-to-end against a real headless Chrome
make check            # fmt + vet + lint + test — what CI runs
```

You need Go 1.23+ and, for `make lint`/`make fmt`,
[golangci-lint v2](https://golangci-lint.run/welcome/install/). Integration
tests need Chrome or Chromium; point `TETHER_CHROME_BIN` at it if it isn't in
a standard place. On Linux CI-like sandboxes you may also need
`TETHER_CHROME_ARGS=--no-sandbox`.

Optional: `pre-commit install` runs the formatter and linter before each commit
(see `.pre-commit-config.yaml`).

## Where things live

Read [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) first. In short:

| Package | Responsibility |
|---|---|
| `internal/cdp` | DevTools transport: connect/launch Chrome, tab primitives |
| `internal/session` | One session = one tab; resolution, lifecycle, idle reaping |
| `internal/core` | The engine every face calls: `Do`, `Eval`, `Navigate`, `Policy` |
| `internal/proxy` | HTTP/HTTPS forward-proxy face |
| `internal/webdriver` | W3C WebDriver face |
| `internal/mcp` | MCP face and stdio bridge |
| `internal/remote` | Planned face (stub) |
| `internal/server`, `internal/api` | Listener, auth, guard, control API |

Faces stay thin. If a change needs browser logic, it almost certainly
belongs in `core` or `cdp`, behind the existing interfaces, so every face gets
it.

## Pull requests

- Keep PRs focused; one logical change each.
- Add tests. Table-driven where it fits; `httptest` for HTTP faces; the
  `cdptest` fakes for anything above the CDP layer.
- Update docs and the `[Unreleased]` section of `CHANGELOG.md`.
- Use [Conventional Commits](https://www.conventionalcommits.org/) style
  subjects (`feat(proxy): …`, `fix(session): …`) — release notes are built
  from them.

### Security checklist for reviewers and authors

- Does this add a way to reach the browser without the token?
- Could anything new end up in logs (headers, cookies, URLs with query
  strings)? Use `logx.SafeURL` and keep secret-bearing keys out of attributes.
- Does it bypass `core.Policy` for any navigation?

## Reporting security issues

Please don't open public issues for vulnerabilities — see [SECURITY.md](SECURITY.md).

By contributing you agree that your contributions are licensed under the MIT
License and that you'll follow our [Code of Conduct](CODE_OF_CONDUCT.md).
