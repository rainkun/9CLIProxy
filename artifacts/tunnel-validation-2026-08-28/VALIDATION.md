# CLIProxyAPI tunnel validation

Validated on Windows x64 (Intel x86-64 compatible) on August 28, 2026.

- Server build: `cliproxyapi-windows-amd64.exe`
- Desktop build: `cliproxyapi-desktop-go-windows-amd64.exe`
- Tunnel mode tested: Cloudflare Quick Tunnel, direct relay mode
- `GET /api/health`: HTTP 200 (`{"ok":true,"status":"ok"}`)
- `GET /v1/models` with API key: HTTP 200
- Restart reused the same `shortId` and cloudflared process (one process only)
- Disable stopped cloudflared; no test server or cloudflared process remained
- Focused tunnel tests, race test, API tests, and `go vet` passed

The external `https://abc-tunnel.us` relay was separately observed returning
HTTP 530 during validation; this is an external relay availability condition,
not a local CLIProxyAPI process failure.
