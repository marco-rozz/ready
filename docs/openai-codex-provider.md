# OpenAI Codex OAuth provider

`provider: openai-codex` is an experimental READY-owned integration for using
ChatGPT Codex subscription entitlement through an Eino `AgenticModel`. It is a
different trust and billing boundary from an OpenAI Platform API key:
`openai-codex` accepts no `api_key`, never contacts `api.openai.com`, and never
falls back to a paid provider unless that provider is explicitly present later
in `llm.failover`.

Each named provider instance has an independent credential. Authenticate with:

```text
ready login llm:codex-work --agent-dir /path/to/agents/casa
ready login llm:codex-work --agent-dir /path/to/agents/casa --device-code
ready login llm:codex-work --agent-dir /path/to/agents/casa --status
ready login llm:codex-work --agent-dir /path/to/agents/casa --forget
```

Browser login uses Authorization Code + PKCE and a registered loopback callback
on `localhost:1455`. Device authorization is intended for headless hosts and may
need to be enabled in the ChatGPT account or workspace. `ready serve` is always
non-interactive: missing, expired-without-refresh, revoked, or malformed
credentials make only that provider candidate unavailable and name the login
command to run.

Credentials live in the selected agent's encrypted Secret store
(`<agent-dir>/secrets.age`, key in `secrets.key` by default; `oauth.path` and
`oauth.token_store` are removed keys), one entry per provider with its own
generation. LLM entries use the `llm:` namespace and MCP servers `mcp:`, so an
MCP server and LLM provider with the same name cannot collide. A release that
kept them in `oauth/llm-*.json` is not imported: log in again. Access tokens, refresh tokens, authorization
codes, PKCE verifiers, and state values are never emitted by status or logs.
Rotated refresh tokens are persisted before use; compare-before-save protects a
new out-of-band login from an older daemon refresh.

Model traffic defaults to `https://chatgpt.com/backend-api/codex/responses`.
`base_url` may override only that model-service base (primarily for contract
tests); OAuth hosts and public-client metadata remain fixed. Requests are
stateless (`store: false`), streaming, account-scoped, and carry native function
tools. Auth/account headers are removed on a cross-origin redirect. The only
transport-local replay is one 401 refresh before the response stream begins;
rate limits, quota, overload, model, request-shape, and general transport
failures return to READY's normal orchestrator.

The ChatGPT backend wire contract is not documented as a public OpenAI Platform
API and may change. READY isolates it in `internal/codex` and
`internal/codexauth`, pins it with deterministic request/SSE/OAuth fixtures, and
keeps the configured failover chain authoritative. Model IDs are operator
supplied and are not rewritten or checked against a hardcoded catalog.

## Provenance

- The [official OpenAI authentication documentation](https://developers.openai.com/codex/auth)
  establishes ChatGPT browser login, automatic refresh, credential-storage
  sensitivity, the subscription-versus-Platform boundary, and headless device
  authorization.
- Public-client endpoint/header/body/event behavior was independently
  cross-checked on 2026-09-29 against the locally installed OpenAI Codex CLI
  `0.158.0` and encoded as READY-owned HTTP fixtures. No Codex CLI, Pi, `pi-go`,
  or companion process is imported, linked, launched, or required at runtime.

Rollback is configuration-only: remove every `openai-codex` reference from
`llm.default`/`llm.failover`, select an existing provider, then optionally remove
the inert secret with `ready login llm:<provider> --agent-dir <dir> --forget`.
