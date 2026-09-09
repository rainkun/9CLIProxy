# Model Combos

This directory owns the maintained Combo page for the single-file management UI.
The Go implementation lives in `sdk/api/handlers/combos*.go`; configuration and
management endpoints live in `internal/config/combos.go` and
`internal/api/handlers/management/combos.go`.

## User workflow

1. Open **Model Combos** in the management sidebar.
2. Name the combo, then select Fallback, Round Robin, or Fusion.
3. Select a provider and search its registered models. Add models with **+**.
4. Reorder members with the up/down buttons. First position is highest fallback priority.
5. For Fusion, select a judge provider/model. Review the N+1 call warning.
6. Save. Clients can use the combo name as their `model`.

The model picker uses authenticated `GET /v0/management/combos/models`, populated
from the live model registry without exposing API keys or contacting providers.
Selecting a model stores `provider::model` to avoid ambiguity across providers.
Existing bare IDs and user-defined slash prefixes continue to work. Custom IDs
remain under Advanced. Editing preserves unavailable selections so temporarily
offline credentials do not erase saved configurations.

## Strategy contracts

| Strategy | Behavior |
| --- | --- |
| Fallback | Try members in configured order until one succeeds. Retry eligible upstream/auth/rate-limit errors, not generic malformed requests or caller cancellation. |
| Round Robin | Rotate the starting member, then use the same failover policy. Sticky requests defaults to 1. Rotation is synchronized within the server process; independent processes have independent counters. |
| Fusion | Start one non-streaming execution per panel member concurrently. Gather usable text in configured order, then ask the selected judge to synthesize one answer from the original request and candidates. |

Fusion makes N+1 logical executions on success. Normal provider-level credential
failover/retries may increase actual upstream calls. Every panel call and judge
call retains the normal usage-reporting path; response `usage` is the judge's
usage, not an aggregate bill. `X-CLIProxy-Fusion-Calls` reports logical executions.
`X-CLIProxy-Fusion-Panel-Successes` identifies partial panels.

Fusion supports OpenAI Chat Completions, Responses/Codex, Claude Messages, and
Gemini GenerateContent text synthesis. Streaming waits for the panel, then
forwards only judge output; it does not reveal intermediate candidate answers.
The combo name is retained in normal responses and streamed model fields.
No failover occurs after output has been committed.

Tools/tool continuations, media generation, multiple output candidates,
stateful Responses, and unsupported protocols are rejected before fan-out.
Multimodal user content is preserved for provider handling, but outputs must be
text. There are at most 16 panel models; an empty or over-64-KiB panel answer is
not considered usable. `min-successful-models` defaults to 1; below this threshold
the judge is not called. Judge failure is returned as an error, not a panel
answer masquerading as synthesis. Cancellation reaches every branch. Token-count
requests count the first usable member without generation or rotation.

The code rejects nested combos to keep fan-out and costs understandable.
Unknown strategies and invalid judges/thresholds are rejected rather than
silently converted to Fallback.

## Build and verification

```powershell
bun ui/combos/build.mjs
bun test ui/combos
go test ./internal/config ./sdk/api/handlers ./internal/api/handlers/management -run 'Test.*(Combo|Fusion)'
go test -race ./sdk/api/handlers ./internal/api/handlers/management -run 'Test.*(Combo|Fusion)'
go build -tags production -ldflags="-H=windowsgui" -o bin/cliproxyapi-ui-windows-amd64.exe ./cmd/desktop
```

The repository currently vendors compiled `management.html`, not a complete
frontend source checkout. `build.mjs` compiles this JSX module and replaces only
the Combo page through the existing React/API/store adapter. It wraps the module
in a private scope, preserves all other screens, and updates both
`static/management.html` and `internal/desktop/management.html` identically. It is
idempotent and fails when required upstream adapter markers are missing. Recheck
the adapter and browser smoke test when replacing the upstream bundle.

The original Combo view remains unused as `legacyCombosPage`; routing invokes the
new page. No second React dependency or runtime is introduced. Supported bundle
locales are English, Simplified Chinese, Traditional Chinese, and Russian; the
page also includes Vietnamese for host UIs that enable that locale.

## Reference

The implementation was informed by the retry/fallback and model-group separation
in [LiteLLM's router](https://github.com/BerriAI/litellm/blob/litellm_internal_staging/litellm/router.py),
reviewed on 2026-09-09. It is an independent Go implementation, not a Python
dependency or a claim that LiteLLM exposes this exact Fusion strategy.
