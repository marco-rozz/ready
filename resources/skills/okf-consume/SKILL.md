---
name: okf-consume
description: Answer questions from the agent's Open Knowledge Format (OKF) bundles under okfs/, a directory of Markdown files with YAML frontmatter, and report the provenance, trust tier, freshness and attestation status of every concept used. Use this skill when the user asks what a metric, table, policy or procedure means according to a knowledge bundle, asks which concepts are stale, deprecated or unverified, or asks for a value that a bundle defines with an Attested Computation. Requires the okf_query tool.
license: Apache-2.0
metadata:
  version: "0.1.0"
  publisher: google
  adapted-by: ready
---

# Answer from an OKF bundle

This skill only reads bundles, and only through the `okf_query` tool. **Prerequisite:** `okf_query` must be in your tool list. If it is not (the active profile denies it), say that knowledge bundles are not available and stop. Creating or changing a bundle is out of scope for this skill: use the `okf-generate` skill for that.

It answers questions from OKF v0.3 bundles and reports how far the reader can trust each concept used. [references/trust.md](references/trust.md) gives the exact trust, freshness, conflict, path and value rules; `okf_query` computes the mechanical parts for you (tier, status, freshness, path and link resolution, citation lookup, conflict ordering, conformance), so you do not recompute them by hand.

## Rules

1. **Answer from the bundle.** If the bundle does not cover the question, say so. Label any statement from general knowledge as general knowledge.
2. **Report trust.** For each concept you use, report its trust tier, its status, and whether it is stale.
3. **Do not write your own computation or executor.** READY cannot run computations. If an Attested Computation defines a value, never write or run a substitute query; see step 6.
4. **Do not reject content.** Use a concept even if it has unknown keys, an unknown `type`, missing optional fields, or broken links. A low trust tier is advice, not a reason to refuse.
5. **Do not change the bundle.** `okf_query` never writes. **Human confirmation is recorded only by the human, through the `/verify` session command.** When a user asks you to mark a concept as reviewed, or says they confirm it, write nothing: tell them to run `/verify <bundle>/<concept>`, which shows them the raw content and a token and records their verification when they send the confirmation line. Never claim a concept is human-reviewed unless `okf_query` reports that tier.

## Procedure

### 1. Find the bundle

Call `okf_query` with `action: "list"`. It names every bundle under `okfs/` with its `okf_version`, title and description. The description states the scope and when to use it; with more than one bundle, choose by description. If `okf_version` is not `0.3` the tool still reads the bundle best-effort and adds a note. If there are no bundles, say so.

### 2. Find the relevant concepts

Do not read the whole bundle. Use:

- `action: "scan"` with `bundle`: one row per concept with its id, type, trust tier, status, freshness, title and description, after a whole-bundle summary. Filter with `type`, `tag`, `grep` (id, title or whole file text), `current`, `stale`, `deprecated`, `unverified`; filters combine with AND. At most 50 rows are shown; narrow with filters when told to.
- `action: "index"` with `bundle` (and optionally `dir`): the tree of directories and concepts, when you need to see how the bundle is organised.

Before you answer, scan again with `grep` for other concepts on the same topic, so that you find any conflict.

### 3. Read each concept

Call `action: "read"` with `bundle` and `path` (the concept id from scan, or a bundle-relative file path such as `tables/index.md`). For a concept the result starts with a **trust header** (qualified id, type, tier with notes, status with the resolved replacement, freshness, change time and actor), then the file, then the **resolved links, path fields and citations**.

- A link marked `external (not read)` points to a URL or outside the bundle: do not try to read it. A link marked `missing` is missing knowledge, not an error.
- Follow links that relate to the question with further `read` calls.
- Long files continue with the `offset` the response gives.

### 4. Judge trust and freshness

Use the header values; do not recompute them.

1. **Trust tier** (`unverified`, `machine-confirmed`, `human-reviewed`). If the notes say the concept was verified only before its last change, say so. A note that `generated.at` is malformed means currency cannot be established: report the concept as unverified.
2. **Status.** `draft`: say it is a draft. `deprecated`: do not present it as current; read the replacement the header names (if none resolved, look for it in the links and body, and say that you did). Any other status value: not current, show it raw.
3. **Freshness.** Say when a stale concept became stale. Stale means "check again", not "false".
4. **Sources.** Weigh claims with the source facts: `author` shows authority, `usage_count` over `usage_window` shows liveness (compare by order of magnitude only), `last_modified` shows recency.

### 5. Resolve conflicts

If two concepts disagree, call `action: "compare"` with `refs` (two to ten qualified references `<bundle>/<id>`). It orders them by OKF precedence and names the step that decided each pair: status, then trust tier, then freshness, then recency. Whether the concepts actually disagree is your judgment; the tool only orders them. Tell the user about the conflict and the choice. When the tool reports `undecided`, show both answers and name the concept that supports each.

### 6. Values defined by an Attested Computation

If the question asks for a value that an Attested Computation produces, `read` the computation concept. The tool reports `runtime`, the parameters (name, type, required), where the computation is (a file or the first block of its `Computation` section, shown inline), the executor and receipt fields, and whether an attester exists. **READY never executes a computation, executor or attester.** So:

- Never write, edit or run a query or script to obtain the value, and never present a value as attested.
- Show the computation as the bundle gives it, with the user's parameter values bound where they are clear (ask the user for any required value that is not), say that it did not run and that its result is therefore not attested.
- If there is no computation, say the concept is not attestable. If `runtime` or `attester` is absent, say a value from it could never be shown as attested.
- Report `verified` (the definition was confirmed) and attestation (a run used the sanctioned computation) separately; here attestation is always "not attested".

### 7. Answer

Give the answer first. Then list, using the OKF terms exactly as written:

- **Concepts used:** each concept id with its trust tier, status and freshness (and `not attested` for a value from an Attested Computation).
- **Citations:** for each footnote `[^id]` that supports the answer, the `title` and `resource` of the matching `sources` entry (`read` lists them).
- **Gaps:** the parts of the question the bundle does not cover.

## Check a bundle

When asked whether a bundle is well formed, call `action: "validate"` with `bundle`. It reports the conformance findings (errors fail conformance; warnings are fixable conventions; info is observational) with file and line, and the verdict `conformant` or `not conformant`. Report them; do not fix anything.

## Older bundles

For a bundle declaring `okf_version` 0.2 or earlier, or none, `okf_query` applies the older-version fallbacks itself: a path field without a leading `/` that does not resolve from its file is resolved from the bundle root (the output marks it), and `timestamp` stands in for a missing `generated.at`. A concept with no trust fields is unverified but still usable.
