# The llm block — providers, credentials, failover

## Shape

A provider **instance** is one entry of `llm.providers` (the map key is the
instance name, and it must not contain `::`). A model is referenced
anywhere as `<provider-instance>::<model-id>`, split at the **first** `::`
— so an Ollama model id like `gemma4:31b` survives whole.

```yaml
llm:
  default: "ollama-gemma::gemma4:31b"    # required, model reference
  failover:                              # optional ordered chain
    - "or-gemma::google/gemma-3-27b-it"
  providers:
    ollama-gemma:
      provider: "ollama"    # provider TYPE; the map key is the instance name
      api: "ollama"         # REQUIRED on every instance, no implicit default
      base_url: "http://localhost:11434"
      context_window: 8192
```

Full key spellings and the complete commented example live in
`config.example.yaml`; the validation rules below are stated from
`internal/config`.

## Chain semantics

- Effective chain = `[default, failover...]` in declared order. Duplicates
  across default+failover are rejected; empty model ids are rejected.
- The orchestrator owns per-candidate health in memory (reset by restart).
  A candidate becomes active only after one of its calls or probes succeeds.
- Changing provider mid-turn (failover) never re-runs recorded tool calls:
  every attempt resumes from the live history's last complete tool-call
  group, and a restart is refused once content was already delivered.

## Per-type rules

| Provider type | Valid `api` | Credential |
|---|---|---|
| `ollama` | `ollama` | optional (Bearer when set) |
| `openrouter` | `chat_completions` | `api_key` or `api_key_file` required |
| `opencode` | `chat_completions` \| `anthropic` \| `google` \| `responses` | required on all four |
| `openai-codex` | `responses` only | **forbidden** — authenticate via `ready login llm:<name>` |

Details worth knowing:

- `api` is mandatory everywhere; there is no default protocol.
- `api_key` and `api_key_file` are mutually exclusive; `_file` content is
  trimmed; an empty-after-trim file is an error.
- Default `base_url` values: ollama `http://localhost:11434`, openrouter
  `https://openrouter.ai/api/v1`, opencode `https://opencode.ai/zen/v1`,
  openai-codex `https://chatgpt.com/backend-api`.
- **opencode `google`**: point `base_url` at the gateway **root**; the SDK
  itself appends `/v1beta/models/{model}:generateContent`.
- **opencode `anthropic`**: Anthropic Messages protocol, Bearer auth, fixed
  8192 max output tokens.
- **openai-codex**: ChatGPT-subscription identity — a distinct billing
  boundary; needs `api: responses`, refuses api keys at boot (the error
  names `ready login <name>`), never falls back to OpenAI Platform. After
  one 401 the credential is refreshed and the request replayed once.
- **ollama-only keys** on any other type are a boot error:
  `keep_alive` (one duration per instance, applies to every model on it)
  and `options` (`temperature`, `top_p`, `top_k`, `num_predict`, `num_ctx`,
  `seed`, `repeat_penalty`, `num_thread`). Per-model tuning is not
  supported: declare a second ollama instance instead.
- `context_window` may be declared on any provider instance — it is the
  denominator for the chat footer's ctx% and `/context` free space; when
  absent those indicators are absent, never estimated. Ollama only: keep
  `options.num_ctx` equal to `context_window` (a mismatch warns at boot —
  Ollama silently truncates to `num_ctx`).

## Failover knobs

| Key | Meaning |
|---|---|
| `failover_retries` | same-candidate transient retries before demotion/failover; range 0..5 |
| `failover_retry_backoff` | exponential retry base, doubling each time, hard-capped at 30s |
| `failover_attempt_timeout` | budget for the FIRST received chunk of the first model call; `0s` disables; after that chunk only the outer 300s agent deadline applies. Non-streaming candidates instead share a proportional slice of the remaining deadline. |
| `recovery.soft` | cooldown ladder after 429/5xx/overload |
| `recovery.hard` | cooldown ladder after auth, 404 model-not-found, quota, transport, budget expiry |
| `recovery.backoff_factor` | multiplies the cooldown per successive demotion; must be > 1 |
| `recovery.probe_timeout` | bounds each recovery probe (and the boot preflight) |
| `recovery.idle_probe_window` | recovery probes run only when agent traffic happened this recently; an idle daemon never probes |

Failure behaviour by class (what an operator sees):

- Retried-then-failed same candidate: transport/DNS/TLS, attempt-budget
  expiry (hard ladder), EOF, 429, 5xx/overload (soft ladder), and the
  agent-level "empty completion" (soft).
- Advance immediately, no same-retry: 401/403, 404 model-not-found, quota
  exhaustion — all demote hard. Quota demotes **every candidate on the same
  provider instance** (they share the key/budget).
- Neither retried nor advanced: request-shape 400s, caller cancellation,
  outer deadline, unknown errors. The turn just ends for that candidate.

Recovery: a background worker probes cooling candidates (never unproven
ones) in declared order once their cooldown expires and traffic is recent;
the first healthy probe is promoted, always restoring the highest-priority
healthy candidate. A user turn arriving while every candidate is cooling
gets exactly one forced attempt of the highest-priority candidate, even
through its cooldown.

## Boot

- Registry-level mistakes (malformed reference, undeclared provider,
  duplicate reference, empty model id) fail boot.
- Per-candidate construction/credential problems only mark that candidate
  unhealthy with a WARN; the daemon still boots.
- Readiness per candidate: light catalog preflight + an isolated synthetic
  tool-call probe (no user history, no production tools, no HA data, no
  side effects). Evaluation stops at the first ready candidate; the rest
  stay `unproven` and are verified by their first real use.
- An unreachable default does **not** fail boot: with no candidate ready the
  daemon starts in the `unavailable` state and recovers via probing; the
  chat status line shows `LLM unavailable` in red.
- Legacy config shapes (`llm.provider` / `llm.model` / `llm.models`) are
  detected and rejected with a migration message naming the new shape.
