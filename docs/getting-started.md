# Getting started

Tether is one binary. Install it, point it at a Chrome, and every client that
speaks HTTP proxy, WebDriver or MCP can use that browser, logged-in sessions
and all.

## 1. Install

=== "Go"

    ```sh
    go install github.com/adeelahmad/tether/cmd/tether@latest
    ```

=== "Homebrew"

    ```sh
    brew install --cask adeelahmad/tap/tether
    ```

=== "Docker"

    Headless Chromium ships inside the image.

    ```sh
    docker run --rm -p 8787:8787 \
      -e TETHER_TOKEN=$(openssl rand -hex 32) \
      -v tether:/data ghcr.io/adeelahmad/tether
    ```

=== "Binary"

    Download an archive for your platform from
    [Releases](https://github.com/adeelahmad/Tether/releases) and put
    `tether` on your `PATH`.

## 2. Start

```sh
tether serve --launch
```

This opens a dedicated Chrome window with a persistent profile. Log in to a
site (say, GitHub) once; the profile remembers it.

!!! tip "Prefer your everyday Chrome?"
    Skip `--launch`. On Chrome 144+, open `chrome://inspect/#remote-debugging`,
    allow remote debugging, and run `tether serve`: it finds the browser
    automatically. On any Chrome you can also start it with
    `--remote-debugging-port=9222 --user-data-dir=<dir>` (Chrome requires a
    non-default profile directory for the debugging port).

## 3. Prove it

In another terminal, fetch a logged-in page through the proxy:

```sh
curl -s http://127.0.0.1:8787/tether/v1/ca.pem -o tether-ca.pem
curl -s -x http://127.0.0.1:8787 --cacert tether-ca.pem https://github.com/settings/profile \
  | grep -o '<title>[^<]*</title>'
```

You get your profile settings page, the one that redirects to a login screen
for everyone else.

## Next steps

<div class="grid cards" markdown>

-   :material-web: **[HTTP proxy](proxy.md)**

    Headers, modes, the control API and console logs.

-   :material-robot-outline: **[MCP for agents](MCP.md)**

    Connect Claude, Cursor or your own agent.

-   :material-language-python: **[WebDriver](WEBDRIVER.md)**

    Point Selenium at Tether and run your scripts.

-   :material-monitor-eye: **[Remote UI](REMOTE.md)**

    Watch and drive any tab from your phone.

-   :material-shield-lock-outline: **[Policies](POLICIES.md)**

    One token per agent, each with its own domain rules.

-   :material-tune: **[Configuration](configuration.md)**

    Every flag, environment variable and exposure option.

</div>
