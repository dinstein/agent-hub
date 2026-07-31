---
description: The two-engine security sweep — Claude and Codex review the tree independently, then fixes are dispatched
argument-hint: [path or theme to audit, or blank for the whole repo]
---

Read `runbooks/security-audit.md` and follow it from step 0. Scope: $ARGUMENTS

If no scope is given, sweep the whole repo by the runbook's step 2. Probe both engines first: with
only one available the run continues and says so, with neither it stops. Steps 1–6 write nothing to
the tree — the reviewers are sandboxed read-only, and a finding nobody can reproduce does not get
fixed.

This wrapper carries no procedure of its own. If a step here would differ from the runbook, the
runbook wins — edit that file instead.
