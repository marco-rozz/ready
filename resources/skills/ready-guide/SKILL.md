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
| `assets/using-ready.md` | The user asks how to run or talk to READY.: `init`/`serve`/`chat`/`login`/`secrets`, TUI keys and `/commands`, exit codes, env vars. |
| `assets/agent-directories.md` | Anything about WHERE READY. runs: the agent directory, `--agent-dir`, the sole config.yaml, locks/duplicate starts, local OAuth, two-agent setups, library vs runtime copies, move/copy semantics. Establish the agent identity FIRST in every config interview. |
| `assets/install-upgrade.md` | Install, upgrade, uninstall, purge, the installer options, the program/library layout. |
| `assets/config-llm.md` | The `llm:` block: provider instances, wire APIs, credentials, context_window, failover and recovery. |
| `assets/config-channels.md` | The `channels:` block: cli vs zmqcli, telegram, serial, formatters. |
| `assets/config-ha.md` | `mcp_servers:`, the `home_assistant:` digital twin, tool profiles, MCP OAuth. |
| `assets/config-runtime.md` | `sessions:`, `compaction:`, `logging:`, `skills:` keys. |
| `assets/config-runtime-tuning.md` | Numeric tuning from runtime evidence: context windows, compaction thresholds, failover timing. |
| `assets/config-timeouts.md` | Every timeout and time-like knob: what it bounds, the ordering invariant, the log line that reveals it fired, and tuning by expectations (before any runtime evidence exists). |
| `assets/config-manager.md` | The user wants to CHANGE the configuration through the agent: staged editing, `!secret` references, `ready secrets`, and the authenticated ZMQ masked secret editor. |
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

1. **Establish the agent directory first**: which agent directory is being
   configured? Default `~/.ready` (`%USERPROFILE%\.ready` on Windows), or an
   explicit one selected with `--agent-dir` (serve and every login mode;
   cwd-independent; the sole filename `config.yaml`). All subsequent advice
   names THAT directory's config and state paths. See
   `assets/agent-directories.md`.
   For a fresh installation, run init first: it creates the config and the
   first copy of installed skills. The installer keeps only a reference
   config in program storage; serve synchronizes later skill updates.
2. **Decide how ready is run**: with a terminal client (`zmqcli`) or
   the stdin CLI (`cli`)? Exactly one of the two may be enabled; `zmqcli`
   is the default and turns on by itself once a shared secret is set (the
   stdin `cli` is off unless enabled explicitly). Telegram comes later.
3. **Then the LLM**: at least one provider (`llm.default`), then whether a
   failover chain makes sense. Ask about budget/privacy (local Ollama vs
   cloud) before proposing models. For a fresh agent, `ready init` asks
   Ollama local/cloud, proposes the corresponding editable host and collects
   the required cloud API key through hidden terminal input; see
   `assets/using-ready.md` and `assets/config-llm.md`.
   If the user says OpenAI, clarify ChatGPT/Codex OAuth versus an OpenAI
   Platform API key before composing the block. For OAuth use
   `provider: openai-codex`, `api: responses`, and omit `base_url`
   and `api_key`; ask about sign-in as described in
   `assets/config-llm.md`.
4. **Then Home Assistant**: the MCP server (tools) and, separately, the
   digital twin (WebSocket mirror + verified commands). They are independent
   policy owners: one can be off while the other is on.
   **Unqualified request ("aggiungi Home Assistant" / "add Home
   Assistant"): propose the digital twin FIRST** — the `home_assistant:`
   block with `enabled: true`, the nested `websocket:` (`url: ws://…
   /api/websocket`, `token: !secret <name>`), and the per-profile tool
   access. The twin is the contractual read SSOT and carries the verified
   commands; the HA MCP server is an ADDITIONAL step the operator must ask
   for explicitly. Never invent a `twin:` key or a flat
   `home_assistant.url` (the strict decoder refuses both — load
   `assets/config-ha.md` for the exact block shape).
   When enabling the twin, explain its read and command tools and ask which
   to enable for each profile: `default`, `admin`, and any other configured
   or intended profiles. Tool access follows each profile's policy.
   Follow the per-profile interview in `assets/config-ha.md`; include the
   agreed `home_assistant.profiles` changes in the configuration diff.
   EVERY policy owner you add — the twin `home_assistant:`, each
   `mcp_servers.<key>:`, `builtin_tools:` — starts the same question for
   every profile already defined: which of that owner's tools may this
   profile use (`allow:`), and which must it never see (`deny:`)? Ask one
   profile at a time, propose a recommended starter access, and record the
   decision in the same staged diff. Patterns are GLOB-STYLE, not regex:
   `*` is the only metacharacter (it crosses `/`, e.g. `light/*` matches
   `light/turn_on`), matched case-sensitively against the owner's
   UNPREFIXED wire name (not `ha__light/turn_on`).
5. **Then sessions, compaction, logging** — usually the defaults are right;
   present them as "here is what you already get, override only if needed".
6. **Optional fine-tuning pass — expectations, not numbers.** When the user
   asks to tune the setup (or at the end of a fresh install), ask at most the
   four expectation questions from `assets/config-timeouts.md` (failure
   posture; local vs cloud LLM; home responsiveness; channel audience) and
   derive at most a handful of `key: value` overrides from the answers.
   Every derived value must name the expectation that justifies it; "keep
   the default" is the recommended answer unless the expectation clearly
   says otherwise. Numeric tuning *with* runtime evidence (session files,
   logs) follows `assets/config-runtime-tuning.md` instead.

Interview rules:

- One question per turn. Batch nothing. Offer a recommended answer and the
  reason, then let the user decide.
- Secret values never go into the conversation: pasting a token into chat
  persists it in the transcript permanently. For credentials the daemon file
  needs, write the reference (`!secret <name>`) into the staged block
  IMMEDIATELY — the reference alone completes the block; never wait for the
  value before saving (never say "send me the token") — then let the
  user enter the value in the masked secret editor (red area, opens by
  itself at the end of the turn that saved) or with `ready secrets set <name>` out of
  band. The file never holds a value; the `*_file` keys
  no longer exist.
- When adding an MCP server (or any OAuth-capable component), ASK whether
  the endpoint is OAuth-protected before staging: if yes, include the
  `oauth:` block in the same edit and ALWAYS close with the follow-up
  `ready login` command (see `assets/config-manager.md` and
  `assets/config-ha.md`). When this is the FIRST OAuth-capable component
  of the configuration, also stage the top-level `oauth:` section with
  `redirect_port: 7601` (the fixed loopback callback port for `ready
  login`; frozen into the persisted client registration — keep it stable;
  adjacent to zmqcli's 7600).
- When adding an MCP server — or any other owner with a `profiles` block
  (twin, `builtin_tools`) — remember to ASK which of its tools to enable
  or deny for EACH profile already defined in the configuration
  (`default`, `admin`, …), one at a time, with a recommended answer per
  profile. An owner that does not declare the active profile contributes
  zero tools (fail-closed), so a server added without the interview
  silently exposes nothing. Patterns are glob-style: `*` is the only
  wildcard (it crosses `/`), matched against the unprefixed wire name;
  see the per-profile rules in `assets/config-ha.md`.
- After each agreed block, compose the new YAML section in the style of
  `config.example.yaml` (comments and all), then move to the next block.
- When the change should actually be applied to the selected agent's configuration,
  use the staged-editing flow of `assets/config-manager.md` (read, discuss,
  update, show the diff, ask for explicit confirmation, then save) instead of
  asking the user to paste a whole new file — and never write a secret value
  into the file from chat.
- Always state the save status after proposing or applying a configuration
  change: a draft or staged edit is **not saved** and needs confirmation;
  only a confirmed successful save means **saved**, naming the selected
  agent's `config.yaml` and how it activates (automatically — see below; no
  manual restart). If saving
  failed or the tool is unavailable, say the change is not saved and give
  the next step. Report pending credentials or OAuth login separately;
  saving configuration does not complete sign-in. See
  `assets/config-manager.md` for save outcomes.
- After a config-manager save, describe automatic runtime activation for
  reloadable settings. A `logging.*` change requires a daemon restart; state
  that exception explicitly. On the loopback `ready chat` TUI
  the OAuth login runs AUTOMATICALLY at the end of the saving turn (browser
  flow, no restart, runtime reloads in-process afterwards); name the
  `ready login <target>` command only as the out-of-band fallback. The
  "restart" reminder exists only in the standalone `ready login` CLI
  output and never applies to the post-save flow.
- At the end, assemble the complete file with top-level `config_version: 2`,
  remind the user which agent directory the daemon will use (explicit
  `--agent-dir` > `~/.ready` — cwd-independent; the sole filename
  `config.yaml`), and explain that a confirmed save activates **by itself**
  (post-save activation, no `ready serve` restart): in the local loopback
  `ready chat` TUI the red secret editor opens for pending `!secret` names,
  then each OAuth login (`login_required` target) is completed in the
  browser without leaving the TUI, then the daemon reloads its runtime
  in-process (`── configuration reloaded ──`). On other channels (Telegram,
  remote ZMQ, `--exec`) the runtime reloads at the end of the turn and any
  pending secret or login waits for the local TUI (or `ready secrets set` /
  `ready login`). Only `channels.zmqcli.bind`/enabled changes rebuild the
  listener a moment after the reply; `logging.*` changes need a daemon
  restart.

## Maintenance rule

If the READY. source changes a config key set, a validation rule, or a
default, the matching asset file must be updated in the same task. When
advising on a numeric default, verify it against `internal/config` before
trusting this text.
