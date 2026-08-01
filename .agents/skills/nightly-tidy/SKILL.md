---
name: nightly-tidy
description: Run AgentHub's recurring simplification pass to reduce accidental complexity, improve code shape, and reconcile documentation with behavior. Use for a nightly tidy or a focused cleanup slice.
---

# Run the nightly tidy

Read `runbooks/nightly-tidy.md` completely and follow it from step 0. Apply any slice supplied by the
user. If no slice is supplied, choose one using the runbook's selection step.

Landing nothing is a correct outcome. Every change must name the failure it prevents or the reader
it helps. Treat the runbook as the only procedure; if this file differs, follow and update the
runbook.
