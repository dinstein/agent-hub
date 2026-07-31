---
description: The recurring tidy pass — simplify the logic, refactor the shape, make docs and code agree
argument-hint: [slice to tidy, or blank to pick one]
---

Read `runbooks/nightly-tidy.md` and follow it from step 0. Slice: $ARGUMENTS

If no slice is given, pick one by the runbook's step 1. Landing nothing is a correct outcome — every
change must name the failure it prevents or the reader it helps.

This wrapper carries no procedure of its own. If a step here would differ from the runbook, the
runbook wins — edit that file instead.
