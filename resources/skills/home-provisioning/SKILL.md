---
name: home-provisioning
description: "Conventions for CREATING Home Assistant smart objects (helpers, template binary sensors, automations) under the `edit` MCP profile. Use this skill whenever the user asks to build, add, or modify HA structure: threshold/derivative/template helpers, alarm sensors, or detection logic. It layers READY. conventions on top of Home Assistant best practices and tells you when to author a matching local alert rule."
version: "1.0"
---

# Home Provisioning (edit mode)

This skill governs **creation and modification** of Home Assistant smart objects.
It is injected only under the `edit` MCP profile, where the full ha-mcp tool
catalog (helpers, template sensors, automations, YAML) is available. Under the
`default` profile these tools are not exposed and provisioning is impossible by
design — confirm with the user that the daemon is running `--profile edit`
before attempting structural changes.

These rules apply **even when the user does not state them**. The user describes
intent ("alert me when the freezer warms up"); you translate it into a correctly
shaped, discoverable, stable HA object plus — when it is an alarm — a matching
local alert rule.

## 1. Tag every created object with `READY.`

Every object you create MUST be discoverable as READY-owned so it can be
audited, reconciled, or removed later.

- Prefix the **friendly name** with `READY.` (e.g. `READY. Freezer Over-Temp`).
- Where the tool accepts labels/tags, add a `READY` label as well.
- Never silently delete or rename an object you did not create. On drift
  (an expected READY object is missing or differs), **reconcile and warn** the
  user — do not auto-delete foreign objects.

## 2. Helper hierarchy — prefer the least brittle mechanism

Choose the creation mechanism in this priority order:

1. **Built-in helper** (threshold, derivative, min/max, statistics, utility
   meter, schedule) — first choice. These are robust, UI-visible, and survive
   restarts cleanly.
2. **Template Helper / Template binary_sensor** (via UI or API) — when built-in
   helpers cannot express the logic. Embed composition and hysteresis here.
3. **YAML** — last resort only, when neither of the above can express the need.
   YAML changes are the most error-prone and the hardest to reconcile.

## 3. Always set `device_class`

Set an appropriate `device_class` on every sensor/binary_sensor you create
(`temperature`, `humidity`, `motion`, `door`, `problem`, `safety`, `battery`,
etc.). The local digital twin uses `device_class` to interpret signals and the
alert engine relies on it for routing. An untyped sensor is a latent bug.

## 4. Encode stability INSIDE the HA object (division of labor)

The local READY alert engine owns **correlation, ω-weighting, dedup,
confirm-window/cooldown, escalation, and routing**. The HA object owns **stable
detection**. Push the noisy, stateful detection logic down into Home Assistant
so the twin observes a *clean, stable* binary/derived signal:

- **Hysteresis / thresholds**: use a threshold helper with distinct
  `upper`/`lower` bounds (or template logic with an explicit dead-band) so the
  signal does not chatter around a single setpoint.
- **Debounce**: use `for:` durations on template binary sensors so a transient
  spike does not flip the state.
- **Composition**: combine multiple inputs (e.g. "door open AND temperature
  rising") inside a single template binary_sensor rather than emitting raw
  sensors and correlating downstream.

Goal: the binary_sensor the twin sees is already a trustworthy "condition true /
false", not a raw reading the engine must threshold itself.

## 5. After creating an alarm object, author the matching alert rule

When the object you created represents an **alarm condition** (a binary_sensor
that turns `on` when something needs attention), immediately author the matching
local alert filter rule via the `alert-rule-set` tool (available only under
`edit`). Without a rule, the stable signal exists but nothing routes it.

The rule should reference the object's `entity_id`, set urgency/weight
appropriate to the hazard, and choose the delivery lane:

- **Safety lane** (deterministic) for life/property hazards — smoke, gas, water
  leak, freezer/fridge failure with spoilage risk.
- **Contextual lane** (LLM-mediated) for advisory conditions where phrasing,
  quiet-hours, and suppression matter.

Confirm the entity_id with the user or via `homeassistant/get_state` before
writing the rule — never guess it.

## 6. Verify before reporting success

After any create/modify call, confirm the object exists and reports the expected
state (e.g. `homeassistant/get_state` on the new `entity_id`) before telling the
user it is done. Report the created `entity_id`, its `device_class`, the
detection parameters you chose, and the alert rule (if any) you authored.

---

**Goal**: every provisioned object is `READY.`-tagged, typed with a
`device_class`, built with the least-brittle mechanism, stable at the HA layer,
and — when it is an alarm — backed by a matching local alert rule.
