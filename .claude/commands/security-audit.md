---
description: The security sweep — a workflow of parallel finders, adversarial verifiers and one adjudication pass, then fixes are dispatched
argument-hint: [path or theme to audit, or blank for the whole repo]
---

Read `runbooks/security-audit.md` and follow it from step 0. Scope: $ARGUMENTS

If no scope is given, sweep the whole repo by the runbook's step 1. Run it as a `Workflow` — this
invocation is the opt-in. Do not pin a model anywhere; the workflow inherits the session's. Probe
`codex` first: present, it runs the same shards as a second independent engine; absent, the sweep
continues single-engine and says so. Steps 0–3 write nothing to the tree, and a finding nobody can
reproduce with a failing test does not get fixed.

This wrapper carries no procedure of its own. If a step here would differ from the runbook, the
runbook wins — edit that file instead.
