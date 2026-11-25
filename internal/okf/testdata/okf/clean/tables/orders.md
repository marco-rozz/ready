---
type: BigQuery Table
title: Orders
description: One row per order.
tags: [sales]
status: stable
resource: /data/orders.csv
generated: { by: reference_agent/gemini-2.5-pro, at: "2026-06-20T14:00:00Z" }
verified:
  - { by: "human:marco", at: "2026-06-25T09:00:00Z" }
sources:
  - id: erp
    resource: https://erp.example/orders
    title: ERP orders
stale_after: "2026-12-31T00:00:00Z"
---

# Definition

Orders come from the ERP.[^erp] See [revenue](../metrics/revenue.md).
