---
name: security-audit
description: Run AgentHub's security sweep with parallel finders, adversarial verification, adjudication, and a report before fixes. Use for a whole-repository audit or a security review of a named path or theme.
---

# Run a security audit

Read `runbooks/security-audit.md` completely and follow it from step 0. Apply any path or theme
supplied by the user; otherwise audit the whole repository using the runbook's scope-selection step.

The explicit invocation opts into the workflow. Do not pin a model: inherit the active session.
Probe for the optional `codex` CLI as directed. Steps 0-3 write nothing to the tree and end at a
report; do not fix anything until the user names the findings to fix, and do not fix an
unreproducible finding.

Treat the runbook as the only procedure. If this file differs, follow and update the runbook.
