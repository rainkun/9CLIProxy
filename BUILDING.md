# Build on another machine

## Requirements

- Git with access to this private repository.
- Go 1.26 or newer; `go.mod` is the source of truth for the minimum version.
- Windows desktop: Windows and Microsoft Edge WebView2 Runtime.
- Bun is optional for rebuilding the Combo UI or running its tests. The checked-in
  management HTML is ready to build into the desktop executable. The UI was
  verified with Bun 1.3.14.
- An internet connection for the first `go mod download`.
- A C compiler is required for `go test -race`. It is not needed for the
  standalone server build with `CGO_ENABLED=0`.

Clone this repository using the authenticated HTTPS or SSH clone URL shown in
GitHub. Run the commands below from its root. To update an existing clone of
the personal repository:

```sh
git switch main
git pull --ff-only origin main
go mod download
```

Do not change the module path from `github.com/router-for-me/CLIProxyAPI/v7`.
That is the Go import namespace, not the Git push destination.

## Windows desktop

Use PowerShell. Keep all production output under `bin/`.

```powershell
New-Item -ItemType Directory -Path bin -Force | Out-Null
go mod download
go build -trimpath -tags production -ldflags "-s -w -H=windowsgui" -o bin/cliproxyapi-ui-windows-amd64.exe ./cmd/desktop
if ($LASTEXITCODE -ne 0) { throw "Desktop build failed" }

# Create local config only when it does not already exist.
if (!(Test-Path -LiteralPath bin/config.yaml)) {
  Copy-Item -LiteralPath config.example.yaml -Destination bin/config.yaml
}
```

Before running, edit `bin/config.yaml`: replace the example `api-keys`, configure
your providers/auth directory, and use `host: "127.0.0.1"` for the desktop.
Do not commit this file. For an explicit config path:

```powershell
& ./bin/cliproxyapi-ui-windows-amd64.exe --config ./bin/config.yaml
```

The desktop executable contains the full management HTML, including Model
Combos. No separate frontend server or adjacent HTML file is required.

**Desktop is local-only:** its startup code forces non-loopback hosts back to
`127.0.0.1`. Setting `host: "0.0.0.0"` in the desktop config is not sufficient
to expose it on a VPS. Use the standalone server below for direct remote access.

## Windows standalone server

```powershell
New-Item -ItemType Directory -Path bin,bin/static -Force | Out-Null
go mod download
go build -trimpath -ldflags "-s -w" -o bin/cli-proxy-api-windows-amd64.exe ./cmd/server
if ($LASTEXITCODE -ne 0) { throw "Server build failed" }

Copy-Item -LiteralPath static/management.html -Destination bin/static/management.html -Force
if (!(Test-Path -LiteralPath bin/config.yaml)) {
  Copy-Item -LiteralPath config.example.yaml -Destination bin/config.yaml
}

# Edit bin/config.yaml before starting.
& ./bin/cli-proxy-api-windows-amd64.exe --config ./bin/config.yaml
```

The standalone server loads management HTML from `static/` beside the config
file. When moving the server elsewhere, include `static/management.html`
relative to that config directory, or set `MANAGEMENT_STATIC_PATH` explicitly.

## Linux standalone server / VPS

Build on the target Linux machine:

```sh
mkdir -p bin/static
go mod download
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/cli-proxy-api ./cmd/server
cp static/management.html bin/static/management.html
test -f bin/config.yaml || cp config.example.yaml bin/config.yaml
chmod 600 bin/config.yaml
```

Edit `bin/config.yaml` before starting. In particular:

```yaml
host: "0.0.0.0"
port: 8317
remote-management:
  allow-remote: false
  secret-key: "REPLACE_WITH_YOUR_OWN_STRONG_MANAGEMENT_SECRET"
  disable-auto-update-panel: true
api-keys:
  - "REPLACE_WITH_YOUR_OWN_STRONG_API_KEY"
tunnel:
  enabled: false
```

This is an excerpt to merge into the copied config, not a file containing real
credentials. Provider keys/auth files must be configured locally.

```sh
./bin/cli-proxy-api --config ./bin/config.yaml
```

For direct VPS access, allow the listener through the operating-system firewall
and the hosting provider's security group, restricted to trusted client IPs.
No Cloudflare tunnel is required. `/healthz` is an unauthenticated connectivity
check; AI endpoints such as `/v1/models` require an API key.

Management is separate from the AI API. Keep it local unless remote management
is needed. To manage remotely, set `remote-management.allow-remote: true`,
use a strong management secret, and restrict access with HTTPS and network
controls. Do not send real credentials across the public Internet over plain
HTTP. The management page is `/management.html#/combos`, not the server root.

Keep `disable-auto-update-panel: true` so an upstream UI download cannot
overwrite this fork's customized Combo page.

## Optional: rebuild or change the Combo UI

```sh
bun ui/combos/build.mjs
bun test ui/combos
```

The build updates both `static/management.html` and
`internal/desktop/management.html`. Rebuild the desktop executable afterwards,
or copy the updated static file for the standalone server. The adapter changes
only the Combo page within the existing vendored UI.

## Verification

```sh
go test ./internal/config ./sdk/api/handlers ./internal/api/handlers/management -run 'Test.*(Combo|Fusion)' -count=1
go test -race ./sdk/api/handlers ./internal/api/handlers/management -run 'Test.*(Combo|Fusion)' -count=1
```

Broader regression:

```sh
go test ./internal/config ./sdk/api/handlers/... ./internal/api/... ./internal/desktop -count=1
```

On Windows, the pre-existing
`TestSetLatestReleaseRequestHeaders/sets_GitHub_authorization` test sets both
`GITHUB_TOKEN` and `github_token`, then clears the latter. Windows treats those
environment names as the same variable. This unrelated known test can be
excluded while verifying the rest:

```sh
go test ./internal/config ./sdk/api/handlers/... ./internal/api/... ./internal/desktop -skip '^TestSetLatestReleaseRequestHeaders$' -count=1
```

Only source, unit tests, example configuration and the required management HTML
belong in Git. Runtime config, credentials, `bin/`, `buildtmp/`, and local
`artifacts/` stay on the machine.
