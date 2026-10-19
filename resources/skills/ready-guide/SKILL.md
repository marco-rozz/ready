---
name: ready-guide
description: "How to use and configure READY., the self-hosted home-automation assistant. Load this skill whenever the user asks anything about using READY. (commands, chat client, TUI, roles/commands such as /new or /compact), or about configuring it: config.yaml keys, LLM providers and failover, channels (cli/zmqcli/telegram/serial), MCP servers, the HA digital twin, MCP profiles, OAuth, sessions, compaction, logging, skill sync, install/upgrade, or any boot failure and error they saw. Guides a config interview: one YAML block at a time until a complete, valid configuration emerges."
version: "1.0"
---

# Ready Guide — using and configuring READY.

READY. is a self-hosted home-automation assistant for Home Assistant. It
executes natural-language requests as device commands that are **verified
against the event stream**, not blind tool-call forwarding: every command
returns a per-entity outcome (`applied`, `pending`, `already`, `unchanged`,
`unavailable`, `error`), and a failed turn always tells the user whether
anything may have run. LLM providers (Ollama, OpenRouter, OpenCode, and an
experimental ChatGPT/Codex-OAuth provider) form a failover chain that resumes
a failed turn from the transcript without re-running completed actions.

## How this skill is organised

This SKILL.md is a map. The assets carry the details; load the one you need
(`skills-read-asset` with this skill's name) instead of guessing:

| Asset | Read it when |
|---|---|
| `assets/using-ready.md` | The user asks how to run or talk to READY.: `serve`/`chat`/`login`, TUI keys and `/commands`, exit codes, env vars. |
| `assets/install-upgrade.md` | Install, upgrade, uninstall, the config election chain, the Install-home layout. |
| `assets/config-llm.md` | The `llm:` block: provider instances, wire APIs, credentials, context_window, failover and recovery. |
| `assets/config-channels.md` | The `channels:` block: cli vs zmqcli, telegram, serial, formatters. |
| `assets/config-ha.md` | `mcp_servers:`, the `home_assistant:` digital twin, tool profiles, MCP OAuth. |
| `assets/config-runtime.md` | `sessions:`, `compaction:`, `logging:`, `skills:` keys. |
| `assets/config-runtime-tuning.md` | Numeric tuning: context windows, compaction thresholds, failover timing. |
| `assets/sessions-life.md` | What Sessions, Turns, compaction and usage accounting ARE, from the user's point of view. |
| `assets/troubleshooting.md` | A boot failure, an unexplained WARN, `llm_unavailable`, an MCP error, a stale twin. |

Sources of truth. Defaults and key spellings live in `config.example.yaml`
(the complete commented reference shipped with every install) and, for exact
validation rules, in the `internal/config` package. This skill explains
**choices and interactions**; it repeats a default inline only when the
default itself is a design decision, and states the file to verify against.

## Config interview protocol

When asked to help configure READY., do not dump YAML. Walk the user through
the configuration, one block at a time, in this order:

1. **Decide how ready is run first**: with a terminal client (`zmqcli`) or
   the stdin CLI (`cli`)? Exactly one of the two may be enabled; `zmqcli`
   is the default and turns on by itself once a shared secret is set (the
   stdin `cli` is off unless enabled explicitly). Telegram comes later.
2. **Then the LLM**: at least one provider (`llm.default`), then whether a
   failover chain makes sense. Ask about budget/privacy (local Ollama vs
   cloud) before proposing models.
3. **Then Home Assistant**: the MCP server (tools) and, separately, the
   digital twin (WebSocket mirror + verified commands). They are independent
   policy owners: one can be off while the other is on.
4. **Then sessions, compaction, logging** — usually the defaults are right;
   present them as "here is what you already get, override only if needed".

Interview rules:

- One question per turn. Batch nothing. Offer a recommended answer and the
  reason, then let the user decide.
- Secrets never go inline if the user prefers files: every `*_key` and
  `*_token` config key has a `_file` twin. Prefer the file form.
- After each agreed block, compose the new YAML section in the style of
  `config.example.yaml` (comments and all), then move to the next block.
- At the end, assemble the complete file with top-level `config_version: 1`,
  remind the user which config path
  the daemon will elect (explicit `--config` > `./.ready/config.yaml` >
  `~/.ready/config.yaml`), and remind that config is read **at boot only**:
  changes need a `ready serve` restart.

## Maintenance rule

If the READY. source changes a config key set, a validation rule, or a
default, the matching asset file must be updated in the same task. When
advising on a numeric default, verify it against `internal/config` before
trusting this text.
