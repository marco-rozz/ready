---
name: home-topology
description: "Map of the home's physical layout, device naming, and operational rules. ALWAYS load this skill before interacting with Home Assistant — it tells you how to read states and which entity_id to target for every device."
version: "2.2"
tools:
  read:
    - ha_state_query       # ONLY tool allowed for reading any HA entity state
  write:
    - ha_command           # verified common commands (on/off/toggle/set, covers, climate, media, scenes)
---

# Home Topology & Device Mapping

This skill provides the essential mapping between the physical residence and its digital representation in Home Assistant. It is the primary source of truth for resolving room names, device nicknames, and entity IDs.

---

## ⛔ MANDATORY — ENTITY_ID VALIDATION ⛔

# 🚫 NEVER — EVER — INVENT AN `entity_id`. 🚫
# 🚫 NEVER derive one from the user's words. 🚫
# 🚫 NEVER complete, translate, or "guess" one. 🚫

Home Assistant `entity_id`s (e.g. `light.livingroom_lamp_1`, `switch.plug_main`)
are **opaque machine identifiers**. They are NOT predictable from device nicknames like
"the living-room spotlight" or "the plug next to the TV". Any `entity_id` you did not read verbatim from
one of the authoritative sources listed below is HALLUCINATED and MUST NOT be used.

### 🔒 MANDATORY RESOLUTION SEQUENCE (in order — do not skip steps)

Before calling ANY tool that accepts an `entity_id` parameter (`turn_on`, `turn_off`, `set_*`,
`scene/turn_on`, `call_service`, `set_state`, etc.), you MUST resolve the ID like this:

1. **`assets/custom-notes.md`** — check first if it is in your context; it holds user-defined
   nicknames and overrides and takes precedence over everything else.
2. **`ha_state_query`** — THE AUTHORITATIVE, REAL-TIME SOURCE. Always resolve `entity_id`s
   by calling this tool against the live digital twin. Filter by `area`, `domain`,
   `device_class`, or an `entity_id` glob/substring to enumerate the current candidates.
   Examples:
   ```
   ha_state_query(area="living room", domain="light")             # all lights in the room
   ha_state_query(entity_id="*lamp*")                        # by nickname fragment
   ha_state_query(area="bedroom", domain="switch")         # scoped to an area+domain
   ```
   Use the `entity_id` field of the response VERBATIM in the follow-up write call.
3. **If still ambiguous** (multiple plausible candidates returned by `ha_state_query`), ASK
   the user which one they mean. Do NOT pick a plausible-looking string as a fallback.

> ⚠️ **DO NOT rely on `generated/devices.md` as the ground truth for writes.** That file is
> a periodic snapshot and can be stale between regenerations. It is useful only as an offline
> discovery aid (e.g. to browse the topology when brainstorming with the user). Any actual
> write MUST resolve its `entity_id` through `ha_state_query` in the same turn.

### ⚠️ Failure mode

An action whose effect could not be confirmed comes back as `unchanged` with an
`[UNVERIFIED]` note (the twin collected no qualifying evidence in the verification
window). That is NOT a success — do not tell the user "done" in that case. Read the
current state with `ha_state_query` BEFORE any retry through any path (a sent command
may still be in flight), then either issue a fresh command or explain the situation.
Write-path discipline (modes, outcomes, fallback rules) is defined by the
`home-integration` skill; this file owns only the physical ↔ digital mapping.

---

## Tool Usage Rules

| Operation | Tool | Notes |
|---|---|---|
| **Read any entity state** | `ha_state_query` | **Mandatory** — never use MCP `get_state` |
| **Control devices (common actions)** | `ha_command` | on/off/toggle/set, covers, climate, media, scenes — verified outcomes |
| **Specialist writes** | MCP action tools (when configured & allowed) | Only for requests `ha_command` cannot serve — see `home-integration` |

> ⚠️ **NEVER call MCP `get_state` or any MCP read tool.** All state reads MUST go through `ha_state_query`, which reads from the always-on in-memory digital twin with zero latency and no network round-trip.

> ⚠️ **NEVER use state values from conversation history.** Even if a previous turn already read the state of an entity, **always call `ha_state_query` again** to get the current real-time value. States change continuously — cached or remembered values are unreliable and MUST NOT be used as the basis for decisions or responses.

## Read path

Under the `default` profile, the MCP `get_state`, `get_states`, `get_history`, and `get_live_context` tools are explicitly denied — `ha_state_query` is the **only** read path, enforced by config rather than by convention. Under `edit`, both paths remain available for troubleshooting.

Every `ha_state_query` response carries freshness metadata alongside the entity data:
- `twin_connected` — whether the twin has ever received a WS frame.
- `last_ws_activity_ago_seconds` — seconds since the last inbound WS frame of any kind (health signal).
- `last_state_event_ago_seconds` — seconds since the last `state_changed` event (diagnostic only — a quiet house is not the same as a dead connection).
- `stale` — `true` once `last_ws_activity_ago_seconds` exceeds `stale_threshold_states` (default 60s); entity data is still returned.

Beyond `hard_stale_threshold` (default 300s), the tool refuses to serve data and returns a `twin_stale` error instead — treat this as "the digital twin cannot be trusted right now," not as "no entities matched."

## Operational Workflow

### 1. Reading Entity State

`ha_state_query` is the **only** allowed tool for any state read — and it MUST be called **every time**, even if the same entity was queried earlier in the conversation. **Do not reuse state values from previous turns**: they may be stale by seconds or minutes. Always fetch real-time data before answering or acting.

Examples:

```
ha_state_query(domain="light", area="living room")
ha_state_query(entity_id="sensor.*temperature*", verbose=true)
ha_state_query(only_problem=true)   # find unavailable/unknown entities
ha_state_query(domain="weather")
ha_state_query(domain="sensor", device_class="humidity", area="bedroom")
```

Available filters: `area`, `domain`, `device_class`, `entity_id` (glob or substring), `only_problem`, `verbose`.

### 2. Controlling & Setting Values

Use **`ha_command`** for common write operations — it sends on the twin's own
live WebSocket and verifies each effect through Home Assistant's own command
context:

- Turning on/off lights, switches, plugs → `action=on|off`
- Setting brightness / position / temperature / volume → `action=set` with `params`
- Opening/closing covers → `action=open|close|stop|set`
- Activating a scene → `domain=scene, action=activate`
- Running a script / pressing a button → `domain=script, action=run` / `domain=button, action=press`

The outcomes in the response are the truth about what happened (`applied`,
`already`, `pending`, `unchanged` `[UNVERIFIED]`, `unavailable`, `error`) —
interpret them per the `home-integration` skill. Use `mode=send` for slow
devices and follow up with `mode=verify` + the returned `call_id`.

Writes `ha_command` cannot serve (out of its vocabulary, or the twin write
path down): an equivalent MCP action tool may be used ONLY if one is present
in your catalog and allowed by that server's profile; otherwise report the
action as unavailable. NEVER blindly retry a sent command — read state first.
Specialist writes (input_number, per-device services) keep using MCP when
configured.

### 3. Resolving Devices

When a user names a room or device, follow the **MANDATORY RESOLUTION SEQUENCE** at the top
of this file. Summary:
1. `assets/custom-notes.md` (user overrides — highest priority).
2. **`ha_state_query`** — real-time source of truth. Filter by `area`/`domain`/`device_class`
   or an `entity_id` glob to obtain the current `entity_id` verbatim.
3. If still ambiguous, ASK the user. Do NOT fabricate. Do NOT rely on `generated/devices.md`
   for writes — it is a snapshot and can be stale.

### 4. Area Mapping

The home is divided into named areas (e.g., "Living Room", "Bedroom"). Use `ha_state_query(area="<name>")` to list all entities in a room. Area names support case-insensitive substring matching.

### 5. Climate & Weather

- **Forecasts and general weather**: `ha_state_query(domain="weather")`.
- **Real-time local sensors**: `ha_state_query(domain="sensor", device_class="temperature")`.
- Combine both sources for a complete answer: forecast trend + precise local reading.

### 6. Scenes and Atmospheres

For "mood" or lighting atmosphere requests, query `ha_state_query(domain="scene", area="<room>")` to find available scenes, then activate with `ha_command(domain="scene", action="activate")`.

### 7. Safety & Verification

Before executing irreversible or ambiguous actions:
- Query `ha_state_query` to confirm current state.
- If multiple candidates match (e.g., several lights in a room), list them and ask the user.
- `ha_command`'s `sync` mode already verifies the effect (per-entity outcomes); only re-query when an outcome is `unchanged`/`pending`, before any retry.

## Asset Reference

### `generated/ha-config.md`
Home Assistant instance metadata (name, location, timezone, version). Injected automatically.

### `generated/devices.md`
Periodic snapshot of the device/entity topology generated from the live twin:
- **Physical Devices**: Area, Device, Model, and all associated entity IDs.
- **Virtual and System Entities**: Area, Entity, Platform.

> ⚠️ **Discovery aid only, NOT a ground truth for writes.** This file can be stale between
> regenerations. When you need to resolve an `entity_id` for a tool call, always use
> `ha_state_query` — it reads from the live in-memory digital twin.

### `assets/custom-notes.md`
Manual overrides and human-centric notes:
- Device nicknames (e.g., "The bedside lamp" → `light.bedroom_nightstand`)
- Behavioral notes (e.g., "Sensor X spikes above 40°C — ignore those readings")
- User preferences per room

---

**Goal**: 100% accuracy in mapping user requests to the correct entity. Never guess an `entity_id` — always resolve it in real-time via `ha_state_query`. Never treat `generated/devices.md` as authoritative for writes: it is a snapshot, `ha_state_query` is the live source. Never read state via MCP — always use `ha_state_query`. Never rely on state values from conversation history — always query real-time. Common device control goes through `ha_command` (verified); write-path rules live in the `home-integration` skill.
