# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

@AGENTS.md

The sections below supplement AGENTS.md (which has commands, per-directory layout, and code conventions). Read AGENTS.md first.

## Go module path

Module is `github.com/router-for-me/CLIProxyAPI/v7` — the `/v7` suffix is required on every internal import.

## Request flow

A request flows through several layers; understanding the chain is necessary before touching translators, executors, or the thinking pipeline.

1. `cmd/server/main.go` parses flags, picks a token store (file / Postgres / git / object / `--home` redis control plane), loads `config.yaml`, and either runs an OAuth login subcommand or starts the service.
2. `internal/api/server.go` builds the gin engine and middleware chain: logrus logger → recovery → request-logging (`internal/api/middleware`) → CORS → `homeHeartbeatMiddleware` (gates everything except `/v0/management/*` when home mode is unhealthy) → per-route `AuthMiddleware` from `internal/access`.
3. Route handlers under `sdk/api/handlers/{openai,claude,gemini}` (constructed from a shared `BaseAPIHandler`) accept the client payload. Management routes live in `internal/api/handlers/management/` and are only registered when a management secret is configured.
4. `internal/registry` resolves the requested model. `sdk/cliproxy/auth` (the conductor / scheduler / selector) picks a credential from the pool with round-robin + cooldown.
5. `internal/thinking.ApplyThinking()` normalises reasoning/thinking config into the canonical `ThinkingConfig` (`types.go`), then a `ProviderApplier` writes provider-specific output. Do not break the "canonical → per-provider translation" split.
6. `internal/translator/{provider}` rewrites the client payload into the upstream's protocol and the response back. As AGENTS.md notes: do not make standalone changes to `internal/translator/` — see the permission-check rule there.
7. `internal/runtime/executor/{provider}_executor.go` calls the upstream provider; the Codex WebSocket executor is the one place where post-connection timeouts are intentionally allowed (along with the other exceptions listed in AGENTS.md).
8. Usage accounting flows through `internal/usage` → optional `internal/store/postgresstore_usage.go` (only when `PGSTORE_*` is configured).

## Embedding via the SDK

External programs embed the proxy through `sdk/cliproxy`: build with `Builder` (see `builder.go`), run via `Service` (`service.go`). The conductor / scheduler / selector / persist policy live in `sdk/cliproxy/auth/`. Reference docs:

- `docs/sdk-usage.md` — entry point
- `docs/sdk-advanced.md` — custom executors and translators
- `docs/sdk-watcher.md` — config hot-reload hooks
- `docs/sdk-access.md` — access providers
- `examples/custom-provider` — working integration example

## Extending the server

The legacy route-module and AMP integration were removed in v7. Extend the server through the plugin interfaces under `sdk/pluginapi`, `sdk/pluginabi`, and `sdk/pluginhost`; runtime hosting and lifecycle management live under `internal/pluginhost`. Use the examples under `examples/plugin/` as the reference implementations.

## Storage backend selection

`cmd/server/main.go` picks exactly one backend, in priority order: `--home` (redis control plane) → `PGSTORE_*` → `OBJECTSTORE_*` → `GITSTORE_*` → local file. When `--home` is set, local stores are intentionally disabled and remote model updates are skipped. When loading config from home, `cfg.Port` defaults to 8317 and `UsageStatisticsEnabled` is forced on.

## Pre-PR check

```bash
gofmt -w . && go build -o test-output ./cmd/server && rm test-output && go test ./...
```

AGENTS.md flags the build step as REQUIRED after any Go change.
