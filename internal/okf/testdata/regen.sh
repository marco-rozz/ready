#!/bin/sh
# Dev-only: regenerate golden/ from the pinned reference scripts.
# Needs Python 3 + PyYAML (PYTHON=/path/to/python to override). CI never runs this.
set -eu
cd "$(dirname "$0")"
PY="${PYTHON:-python3}"
NOW="2026-07-01T00:00:00Z"
mkdir -p golden
rm -f golden/*.json
for dir in okf/*/; do
  name=$(basename "$dir")
  "$PY" reference/validate.py "$dir" --json --now "$NOW" > "golden/$name.validate.raw" || true
  "$PY" reference/scan.py "$dir" --json --now "$NOW" > "golden/$name.scan.raw" || true
  "$PY" - "$name" <<'PYEOF'
import json, os, sys
name = sys.argv[1]
for kind in ("validate", "scan"):
    raw = f"golden/{name}.{kind}.raw"
    try:
        with open(raw) as fh:
            data = json.load(fh)
    except (ValueError, OSError):
        os.remove(raw)
        continue
    data.pop("bundle", None) if kind == "validate" else data["bundle"].pop("path", None)
    with open(f"golden/{name}.{kind}.json", "w") as out:
        json.dump(data, out, indent=2, sort_keys=True)
        out.write("\n")
    os.remove(raw)
PYEOF
done
