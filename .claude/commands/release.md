---
description: Cut a release — preflight, version bump, changelog, tag, and check what actually shipped
argument-hint: [version, or blank to choose one]
---

Read `runbooks/releasing.md` and follow it from step 0. Version: $ARGUMENTS

Everything before the tag push is local and reversible; everything after it is public within seconds.
Stop and confirm at that step.

This wrapper carries no procedure of its own. If a step here would differ from the runbook, the
runbook wins — edit that file instead.
