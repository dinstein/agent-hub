# 0007 — The tool commands live under `server`, and both altitudes are spelled identically

> **Status** active · **Behaviour** [conventions.md#command-naming](../conventions.md#command-naming), [subsystems/cli.md](../subsystems/cli.md)

`server tool ls | inspect | allow`, with `server tool allow <server>` and
`profile tool allow <profile> <server>` taking the same `--only | --all | --none`.

The cause of the spelling this replaced was that the command tree disagreed with where the rule is
stored: a top-level `tool` sat in the withheld group, `tool ls` applied no allow list at all, and a bare
`tool allow <server>` meant "expose nothing".

The two layers stay **one vocabulary**, and there is still no `deny` verb at either altitude — allow and
deny answer the arrival of a tool the downstream adds tomorrow in opposite directions.
