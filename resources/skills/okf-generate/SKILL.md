---
name: okf-generate
description: Create, update or deprecate concepts in the agent's Open Knowledge Format (OKF) v0.3 bundles under okfs/, a directory of Markdown files with YAML frontmatter that documents data assets, metrics, policies and procedures for people and agents. Use this skill when the user asks to write, generate, enrich or migrate OKF documents or a knowledge bundle; to document tables, datasets, metrics, APIs or playbooks in OKF; to add provenance, trust, lifecycle or attested-computation fields; or to check a bundle for OKF conformance. Requires the okf_query and okf_write tools. A person confirms content with the /verify command; you never record a verification.
license: Apache-2.0
metadata:
  version: "0.1.0"
  publisher: google
  adapted-by: ready
---

# Write to an OKF bundle

This skill changes OKF v0.3 bundles through the `okf_write` tool and reads them through `okf_query`. **Prerequisites:** both `okf_query` and `okf_write` must be in your tool list. If `okf_write` is not (the active profile denies authoring), say that you cannot write to knowledge bundles and stop. The specification is [`SPEC.md`](https://github.com/open-knowledge-format/open-knowledge-format/blob/main/SPEC.md) in the OKF repository. [references/frontmatter.md](references/frontmatter.md) lists every field. [references/templates.md](references/templates.md) gives a template for each document type.

An OKF bundle is a directory tree. A concept is a Markdown file whose frontmatter has a non-empty `type`. A Markdown file without frontmatter or without `type`, such as a `README.md`, is not a concept. A file whose name does not end in `.md` is an asset. The file names `index.md` and `log.md` are reserved at every level.

To answer questions from a bundle, use the `okf-consume` skill.

**This skill diverges from the upstream `okf-generate` skill in one rule that matters:** you never record a verification. See rule 4.

## What the tool does for you

`okf_write` enforces the mechanical rules, so you do not (and cannot) do them by hand:

- It **stamps authorship**: it writes `generated` (`by: ready/<version>`, the current time, and the model that made the call in `x-ready-model`). Any `generated` or `verified` in your arguments is refused.
- It **re-stamps on every change of any value**, frontmatter or body. A human-reviewed concept that you change is no longer human-reviewed. There is no "format-only" exception.
- It **merges minimally**: the keys you do not set keep their exact text, comments included, unknown keys and the `verified` history among them.
- It **requires `base_digest`** for `update` and `deprecate`: the digest that `okf_query read` printed for the file. A conflict means the file changed since you read it; read it again.
- It **maintains `index.md` and `log.md`** (directory and ancestor indexes, the root index with `okf_version: "0.3"`, and a dated log entry). Never write them yourself.
- It **validates** every file it writes. A write that would introduce a conformance error is refused with nothing written; warnings and infos come back in the receipt.
- It **reports the trust receipt**: the tier before and after, how many verifications stopped being current, and the `/verify` hint.
- It never deletes, renames or moves a file, and never writes assets.

## Rules

1. **Write only what a source supports.** Do not invent columns, partitions, URLs, owners, numbers, policies or review dates. If a fact has no source, omit it or ask the user.
2. **Record provenance.** List the sources of each concept in `sources`. Attach each specific claim to its source with a footnote whose label is the source `id`.
3. **The tool records authorship.** Do not try to set `generated`.
4. **Never record a verification.** `verified` is written only when a person confirms the content with the `/verify` session command; the daemon records it, bound to the exact content the person saw. When a person says they confirm or reviewed a concept, do not write anything: tell them to run `/verify <bundle>/<concept>` and follow the two steps it shows (the command prints the raw content and a token, then they send the confirmation line). Never claim a concept is verified before the receipt or `okf_query` says so.
5. **Never write the computation of an Attested Computation.** Copy it exactly from a sanctioned source, such as an approved SQL file, a dbt model, or a query from its owner. Cite that source. If no sanctioned computation exists, describe the metric without one and report the gap. Do not store an unreviewed query in the bundle unless the user explicitly asks; then store it as a Reference with `status: draft`, never linked as a computation.
6. **Keep what you do not own.** When you update a concept, set only the keys you change. The tool keeps the rest. Do not rewrite text that a person wrote beyond what your source requires.

## Procedure

### 1. Set the scope

1. Call `okf_query` with `action: "list"` to find the bundle. If the bundle exists, `scan` it and `read` its root `index.md` and `log.md` first, and note its `okf_version`. The operator creates bundle directories under `okfs/`; you cannot create a bundle, only concepts inside one.
2. List the sources that you can read. Examples are schema metadata, such as `INFORMATION_SCHEMA` views or an OpenAPI file, documentation pages, runbooks, and notes from the user.
3. Tell the user which sources you cannot read.
4. If the scope is not clear, ask the user which assets or topics to include.

### 2. Plan the tree

1. Plan one concept per file. The concept ID is the file path without `.md`.
2. Group concepts in directories by kind, for example `datasets/`, `tables/`, `metrics/`, `computations/`, `policies/`, `playbooks/`, and `references/`. Reuse the directory names that the bundle already has (`okf_query index`).
3. Use stable lowercase file names (`[a-z0-9._-]`, at most 8 segments), and never two paths that differ only by case. The tool refuses other names.
4. Do not name a concept `index.md` or `log.md`.
5. If you plan more than 10 files, show the planned tree to the user before you write them.

### 3. Write each concept (`create`)

Call `okf_write` with `action: "create"`, `bundle`, `path` (without `.md` if you like), `fields` (the frontmatter) and `body`. For the first concept in a bundle that has no root `index.md`, also pass `bundle_title` and a one-sentence `bundle_description` of the bundle's scope and when to use it; agents use that description to choose a bundle.

1. Put in `fields` a `type`: a short descriptive name, for example `BigQuery Table`, `Metric`, `Playbook`, `Reference`, or `Attested Computation`. Reuse the type names that the bundle already uses.
2. Write `description` as one sentence.
3. If the concept describes an asset, set `resource` to the canonical URI of that asset.
4. Write `tags` as a list, for example `["sales", "orders"]`. A consumer reads a scalar `tags` value as one tag.
5. Write other timestamps (`stale_after`, `sources[].last_modified`) as RFC 3339 date-times with an offset, such as `"2026-06-30T14:00:00Z"`, or as a date, such as `"2026-12-31"`.
6. Follow the syntax rules in [references/frontmatter.md](references/frontmatter.md#syntax-section-4). Do not repeat a key. The tool quotes strings itself.
7. Start the body with one to three paragraphs. Tell what the concept is, its grain or scope, how people use it, and its caveats.
8. Use the conventional headings when they apply: `# Schema` for columns or fields, `# Examples` for usage, and `# Computation` for an Attested Computation.

### 4. Link concepts and write paths

1. When the text names another concept in the bundle, link to it with a standard Markdown link, once per section and not from headings or code.
2. Write links in the body relative to the file, for example `../tables/orders.md`, because that form displays correctly on GitHub.
3. Write path fields (`resource`, `sources[].resource`, `computation`, `executor.resource`, `attester.resource`, `superseded_by`) with a leading `/`, for example `/references/attesters/sql-equality.py`. A path field without a leading `/` resolves from the directory of its file, not from the bundle root.
4. Percent-encode spaces in paths, such as `my%20concept.md`.
5. Link only to concepts that exist or that you create in this run.

### 5. Record provenance and lifecycle

1. Give each `sources` entry a stable `id`, the required `resource`, and a `title`.
2. Add `author` (an actor), `usage_count`, or `last_modified` only when you have the real value. Give each `usage_count` a `usage_window`.
3. List the concept's own `resource` as a source when the content came from it.
4. Cite a specific claim with a footnote, for example `Revenue excludes tax.[^rev-policy]`. Define it as `[^rev-policy]: Revenue recognition policy`. The label must equal a `sources[].id`. Do not use numbered labels such as `[^1]`.
5. Write actors as `<producer>/<version>` or `<scheme>:<id>`, with the schemes `human:`, `team:`, and `process:`. Only `human:` gives the human-reviewed trust tier, and only `/verify` records one.
6. For normal content, omit `status`. A missing `status` means `stable`.
7. If the content is incomplete or needs review by its owner, set `status: draft`.
8. To retire a concept, call `okf_write` with `action: "deprecate"`, `concept`, `base_digest`, the replacement as `superseded_by` when one exists (it must be an existing concept), and the `reason`. The tool keeps the file, its definition and its `verified` history.
9. To move or rename a concept, **create** the concept at the new path and **deprecate** the old one with `superseded_by` pointing at the new one. The new concept is a new file with a new `generated`, so it needs a new verification: tell the user. (The upstream rule that a move keeps `generated` and `verified` does not apply: this tool cannot move files.)
10. Set `stale_after` only when a source has a known review or refresh cycle. Do not guess a date.

### 6. Write an attested computation

Do this step only when you have a sanctioned computation (rule 5).

1. Write the computation as its own concept with `type: Attested Computation`, usually in `computations/`, and set its `runtime`.
2. Write its parameters, computation block, executor, and attester as [references/attested-computations.md](references/attested-computations.md) describes.
3. Link to the computation from each concept that uses its value, for example from a `Metric`.

### 7. Index and log

The tool writes the directory index entries (`* [Title](path) - description`, copied from the concept's frontmatter), the ancestor indexes, the root `index.md` when absent, and the `log.md` entry. Do not edit `index.md` or `log.md`. An entry that a person wrote by hand is left as it is. Read the receipt's maintenance lines; if one failed, the next successful write of that concept repairs an index entry.

### 8. Validate

1. Read the findings in each write receipt: warnings and infos of the files written, and the bundle's counts.
2. Call `okf_query` with `action: "validate"` and `bundle` for the full report. Errors mark conformance failures (section 11.1).
3. Fix each warning, or tell the user why you kept it. Warning `W120`, a file that is not a concept, is correct only for documentation such as a `README.md`.
4. Read the info findings, such as stale concepts and verifications older than the content.

### 9. Report

Tell the user:

- the files that you created or changed,
- the concepts that you marked `draft` or `deprecated`,
- the concepts that now need a new verification, from the receipts, and that the user confirms each with `/verify <bundle>/<concept>`,
- the facts that you could not source,
- the validation result.

## Update an existing concept (`update`)

1. `okf_query read` the concept and keep its `Digest`.
2. Call `okf_write` with `action: "update"`, `bundle`, `concept`, that `base_digest`, and the changes: `set` (top-level keys and their new whole values; to change one `sources` entry, pass the whole list; a `null` value removes the key), `unset` (keys to remove; never `type`), `body` (the full replacement body).
3. Change only what your sources support.
4. Read the receipt. Every change of a value re-stamps `generated`, so a change in meaning needs a new verification: the receipt says so, and you tell the user to run `/verify`. If the result equals the current content after normalization, the tool writes nothing and says `unchanged`.
5. On a conflict, read the concept again and redo the change on its current content.

## Migrate an older bundle

To migrate a v0.1 or v0.2 bundle to v0.3, follow [references/migration.md](references/migration.md), applying each change with `okf_write` `update`. Migration changes values, so each migrated concept needs a new verification.
