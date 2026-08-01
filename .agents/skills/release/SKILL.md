---
name: release
description: Cut an AgentHub release using the repository's preflight, versioning, changelog, tagging, and post-release verification procedure. Use when preparing or publishing a release.
---

# Cut a release

Read `runbooks/releasing.md` completely and follow it from step 0. Use the version supplied by the
user, or choose one using the runbook when none is supplied.

Everything before the tag push is local and reversible; everything after it becomes public within
seconds. Stop and obtain confirmation at that step. Treat the runbook as the only procedure; if this
file differs, follow and update the runbook.
