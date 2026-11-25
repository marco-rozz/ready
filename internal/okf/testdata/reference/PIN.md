# OKF reference pin

Repository: https://github.com/open-knowledge-format/open-knowledge-format
Commit:     527e81aff55992be21507e97004286789126f88c
License:    Apache-2.0 (`LICENSE.md`, covers `SPEC.md` and the skills)

Vendored verbatim (test reference only; the daemon never runs Python):

| File          | Upstream path                                   |
|---------------|-------------------------------------------------|
| `SPEC.md`     | `SPEC.md`                                       |
| `trust.md`    | `skills/okf-consume/references/trust.md`        |
| `scan.py`     | `skills/okf-consume/scripts/scan.py`            |
| `validate.py` | `skills/okf-generate/scripts/validate.py`       |
| `okf-generate/SKILL.md`, `okf-generate/references/` | `skills/okf-generate/` (adaptation baseline for `resources/skills/okf-generate/`) |

`SPEC.md` + `trust.md` are normative for `internal/okf`; the scripts are test
oracles. Deliberate divergences are listed in the `okf-knowledge-bundles`
capability spec and each has a fixture under `../okf/`. Golden outputs are
regenerated with `../regen.sh` (needs Python 3 + PyYAML; CI never runs it).
