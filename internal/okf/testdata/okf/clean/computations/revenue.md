---
type: Attested Computation
title: Revenue computation
runtime: bigquery
parameters:
  - { name: year, type: integer, required: true }
executor:
  resource: /references/run.txt
  receipt: [job_id, executed_sql]
attester:
  resource: /references/attest.py
generated: { by: reference_agent/gemini-2.5-pro, at: "2026-06-20T14:00:00Z" }
---

# Computation

```sql
SELECT SUM(amount) FROM t WHERE y = @year
```
