---
name: home-integration
description: "How to control and verify Home Assistant devices through the digital twin (ha_command, ha_state_query): target selection, command modes, outcome interpretation, fallback rules. Injected automatically when the HA integration is enabled."
version: "1.0"
---

# Home Integration — Controlling the House Through the Digital Twin

This skill governs every write operation (and the reads that support it) on
Home Assistant through the digital twin. The intent vocabulary — which domains
and actions exist — comes from the `ha_command` tool description, NOT from this
skill; never assume an action is available without checking it there.

> **Vocabulary lives in the tool, not here.** The active domains and actions
> are rendered in the `ha_command` description at boot. If a request maps to
> something outside that vocabulary (or to `lock` / `alarm_control_panel`,
> which are never available), do not force it — see **Fallback** below.

## Target selection — never fabricate entity ids

`entity_id`s are opaque machine identifiers. NEVER invent, complete, translate,
or derive one from the user's words. Resolve every target first:

1. **`assets/custom-notes.md`** (via `read-skill-asset` on `home-topology`) —
   user-defined nicknames, highest priority.
2. **`ha_state_query`** — the real-time source of truth. Enumerate candidates
   with `area`, `domain`, `device_class`, `entity_id` (glob/substring), `name`
   (friendly-name substring, accent-insensitive), or `state`/`attributes`
   filters. Use the `entity_id` field of the response VERBATIM.
3. Still ambiguous (several plausible candidates)? ASK the user. Do not pick.

Prefer **explicit `entity_ids`** in `ha_command` once resolved. Use the
selector form (`domain` mandatory + `area`/`pattern`/`name`) for bulk
operations ("turn off all the kitchen lights"), keeping these rules in mind:

- A selector without `domain` is refused.
- Broad selectors are capped (`max_targets`, default 20): a refusal states the
  match count — narrow the selector (add `area`/`name`) rather than retrying.
- `toggle` applies to exactly ONE entity; for sets use `on`/`off`.
- Registry-disabled entities are excluded from selector matches; `unavailable`
  targets are reported per entity, never sent to.
- Groups are de-duplicated: when a group's members are also selected, only the
  members receive the command.

## Command modes and outcome interpretation

`ha_command` modes:

- **`sync`** (default) — send + verify. Use it for responsive devices
  (lights, switches, plugs).
- **`send`** — return immediately with a `call_id`. Use it for SLOW devices
  (covers/curtains, thermostats reaching temperature, media players waking
  up), or whenever the user asks for several commands in sequence without
  waiting. Follow with `mode=verify` + the `call_id` later in the turn.
- **`verify`** — outcomes collected so far for a `call_id`. An unknown or
  expired `call_id` means the verification window is gone: read state
  directly instead.

Per-entity outcomes — interpret them literally:

| Outcome | Meaning | What to say / do |
|---|---|---|
| `applied` | Verified by HA's own context (before → after in the payload) | Report done, with the before → after when useful |
| `already` | The entity was already in the requested state | "It was already on/off" — never claim you changed it |
| `pending` | Transitional state observed (opening, heating…) or verification in flight | Say it's in progress; for `send` mode, follow up with `verify` |
| `unchanged` | `[UNVERIFIED]` — no confirming evidence in the window | NEVER report success. Read state with `ha_state_query` before any retry |
| `unavailable` | The target was unavailable — nothing was sent | Report the device as unreachable |
| `error` | HA refused the call or the connection failed | Report the failure; do not blind-retry |

## No blind retries — ever

A command that was SENT must never be blindly re-issued through any path,
because a retry through a different route can double-execute an effect that is
merely slow to confirm. Whenever an outcome is `unchanged`, `pending`, or a
batch came back partial:

1. `ha_state_query` the entity's CURRENT state.
2. Decide from the live state whether the command actually took effect.
3. Only if it did not, issue a fresh command.

The same discipline applies across channels: if the user repeats a request
that ended `[UNVERIFIED]`, read state first.

## On/off semantics (shared table)

The `state` filter of `ha_state_query` and `ha_command`'s expected states use
one semantic table:

- `light`, `switch`, `fan`, `input_boolean`, `siren`, `humidifier`,
  `automation`, `binary_sensor`: `on`/`off` are literal.
- `cover`, `valve`: `on` means `open`, `off` means `closed`. Querying a cover
  with `state=off` returns closed covers.
- `climate`: `on` = any HVAC mode other than `off`; `off` = `off`.
- `media_player`: `on` = anything but `off`/`standby`; `off` includes
  `standby`.
- Other domains: values are literal.
- Transitional states (`opening`, `closing`, …) are neither on nor off — for
  `binary_sensor` the meaning of on/off depends on `device_class` (e.g.
  `motion`: on = motion detected); the entity's `friendly_name` and the
  attributes tell you which.

## Read filters you can rely on

`ha_state_query` supports, all AND-composed: `area`, `domain`, `device_class`,
`entity_id` glob, `only_problem`, `state` (exact / list / semantic on-off /
numeric comparison like `{">": 25}`), `attributes` (equality or numeric
comparison, evaluated on raw attributes — no `verbose` needed for
`{"hvac_action": "heating"}`), `name` (accent-insensitive friendly-name
substring), `sort` (default `entity_id`; `name`, `state` numeric-aware,
`last_changed`; ascending/descending), `limit` (default 50, max 200 — the
response carries the exact `more` count of omitted matches), and
`group_by: area` (area-less entities under `unassigned`). Default order is
`entity_id` — stable across identical repeated queries.

Always re-read state before acting on it, even within the same conversation:
conversation history holds historical values only.

## Fallback — the twin decides, never the other way around

When `ha_command` returns a refusal (out-of-vocabulary domain/action, inactive
domain, or a write-barrier refusal because the twin is reconnecting or
stale):

1. The refusal tells you that **if an equivalent tool is present in your
   catalog you may use it** — e.g. an MCP action tool, if the HA MCP server is
   configured and its profile allows it. The twin does not know (and never
   names) which MCP tools exist; check your own catalog.
2. If no equivalent tool exists, **report the action as unavailable** to the
   user. Do not improvise a service call, do not ask the user for a PIN.
3. When a command was already sent and is merely unverified, the guidance is
   the opposite: check state with `ha_state_query` first — no retry through
   any path before that read.

## Security domains

`lock` and `alarm_control_panel` are deliberately never actuated by the twin:
their services may require a PIN `code` that would land in transcripts and
logs. Route such requests to the user's own HA interfaces (or an MCP tool if
one exists and the operator accepts the exposure) and never request a PIN in
conversation.
