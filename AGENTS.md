# AGENTS.md

Repo: `cpa-plugin` (fork `hurleychin/cpa-plugin`, upstream `Sliverkiss/cpa-plugin`). Contains a single Go c-shared plugin, `workbuddy/`, that wraps Tencent CodeBuddy (WorkBuddy) as a CLIProxyAPI provider.

## Workbuddy release flow (always follow this order)

1. **Every change bumps the version +1.** Update all three places together:
   - `workbuddy/main.go` → `var version = "X.Y.Z"`
   - `workbuddy/VERSION`
   - `registry.json` → `"version": "X.Y.Z"`
2. Build: `cd workbuddy && CGO_ENABLED=1 go build -buildmode=c-shared -ldflags "-X main.version=X.Y.Z" -o workbuddy.so .`
3. Test: `cd workbuddy && go test ./...` (also `go vet .`; CI runs both + `scripts/validate-registry.py registry.json`)
4. **Do NOT commit or release until the user explicitly says so.** Only commit/push/tag when told. Local code edits + test pass are the stopping point.
5. On release approval: backup current deployed plugin, deploy, restart service, then commit + push fork, tag, push tag:
   - Backup/deploy: `cp /root/cliproxyapi/plugins/linux/amd64/workbuddy.so /root/cliproxyapi/plugins/linux/amd64/workbuddy.so.bak-<oldver>` then copy the new `.so` over it, `systemctl restart cliproxyapi`.
   - Push: `GIT_SSH_COMMAND="ssh -i ~/.ssh/id_rsa -o StrictHostKeyChecking=no" git push fork main` (origin is read-only).
   - Tag must be a **plain version** `vX.Y.Z` (no `<id>-` prefix). CPA pluginstore derives the version from the tag after stripping a leading `v`; prefixed tags fail validation. Tag + push to fork triggers the `Build` GitHub Action → Release with assets `<id>_<ver>_linux_amd64.zip` + `checksums.txt`.
   - Verify: `gh run list --repo hurleychin/cpa-plugin`, `gh release view vX.Y.Z --repo hurleychin/cpa-plugin`, and `curl .../v1/models` shows the plugin models.

## Model discovery

- `models.go` `fetchDynamicModelsFromStorage` merges the personal cli-agent model list (from `copilot.tencent.com/v3/config`) with enterprise `custom:*` models (from `/console/enterprises/<eid>/config/models`).
- Personal list uses WorkBuddy client headers (`User-Agent: WorkBuddy/5.3.13 ...`, `X-User-Id`, `X-Enterprise-Id`, `X-Tenant-Id`, `X-Domain: www.workbuddy.cn`); the cli agent's `models` field is a plain string array enriched from the static `wbModels()` table.
- Results cached globally for 5 min. **A partial merge (either fetch failed) is NOT cached**, so the next model query retries — don't reintroduce caching of incomplete merges.
- The static `wbModels()` table is the **fallback** when the upstream fetch fails. If a real upstream model is missing from `/v1/models`, add it to `wbModels()` so it always shows (e.g. `glm-5.3-flash` was added there after it vanished during a transient fetch failure). Keep `wbModels()` in sync with the upstream cli-agent list; it is NOT auto-populated.

## Upstream content-audit desensitization

- Tencent CodeBuddy blocklists security/abuse keywords and Claude Code brand/git phrases; upstream rejects requests containing them (verified on `hy3`, which audits strictly; `custom:deepseek-v4-flash` does not).
- `desensitize.go`: split flagged terms with a zero-width space U+200B (e.g. `DoS` → `D\u200bS`). Applied **only to `system`-role messages** (`desensitizeContent`) — user content must stay byte-identical.
- `payload.go` `sanitizeBlockedTemplates` is now a passthrough; exact phrase rewrites were removed. The term table is the single source of truth. Terms ending in `)` (e.g. the old `Main branch (…)`) do NOT work with the `\b` regex — add plain two-word terms like `Main branch` instead.
- When auditing sensitivity, test against upstream `hy3`, not `custom:deepseek-v4-flash`.

## Session correlation & trace

- Each chat request is correlated to an upstream conversation via the incoming session identifier. Recognized sources (host-compatible priority order, first non-empty wins): `X-Claude-Code-Session-Id` (Claude Code), `Session-Id` / `Session_id` (Codex), `X-Session-ID`, `X-Session-Affinity` (OpenCode), `X-Client-Request-Id` (pi), then body `session_id` / `sessionId` / `prompt_cache_key` / `conversation_id` (OpenAI/Responses).
- When a session id is present the request is a continuation: stable sha256-derived v4-UUID conversation id, `X-Agent-Purpose: conversation`, 4-part `b3` + `X-B3-ParentSpanId` (continuation parent), `X-CodeBuddy-Request: 1`. Without one it's a fresh topic: random ids, `conversation_topic`, 3-part `b3`, no codebuddy-request. See `newChatSession` / `allSessionCandidates` / `sessionIDFromBody` (trace.go, main.go).
- Session tracing is a **plugin config option**, independent of CPA's request-log/debug. Enable `trace_enabled: true` under `plugins.configs.workbuddy` (optionally `trace_dir`); the plugin writes JSONL to `trace_dir/session-trace.jsonl` (default `/root/cliproxyapi/logs/workbuddy-trace`). Each line records session correlation + redacted outbound headers. Default is off.
- Gotchas: `ExecutorRequest` has **no RequestID field** (don't reference it); trace outbound headers only via `filterTraceHeaders` (redacts Authorization, keeps correlation headers).

## Gotchas

- `gofmt`/`go` may not be on PATH: prefix `export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin`.
- Plugin runs as a c-shared `.so`; SDK is `github.com/router-for-me/CLIProxyAPI/v7` (module cache at `$GOMODCACHE`). `ExecutorRequest` carries no host config.
- Don't create standalone diagnostic `*_test.go`/`.go` files and forget them — remove temp test files after debugging.