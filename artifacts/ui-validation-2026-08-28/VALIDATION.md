# UI validation — 2026-08-28

- Target: Windows x86-64 (`GOOS=windows`, `GOARCH=amd64`)
- Wails build tags: `desktop,production,wv2runtime.download`
- Verified PE machine: `0x8664`
- Verified application window: `CLIProxyAPI Management`
- Verified embedded server: `127.0.0.1:8317`
- Verified management dashboard loads and reaches `Connected`
- Verified `internal/desktop` tests
- Verified local management password survives configuration reload

The temporary stdout/stderr files used during this check were removed after validation.
