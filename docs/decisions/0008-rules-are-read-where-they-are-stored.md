# 0008 — A rule is reported by the resource that stores it; a listing reports the effect

> **Status** active · **Behaviour** [subsystems/cli.md](../subsystems/cli.md)

`server ls` and `server inspect` carry the global allow list, `profile ls` carries a profile's selectors,
and `server tool ls` / `profile tool ls` list the tools each layer leaves offered.

`server tool ls --rules` is hidden and deprecated but still accepted. It was meant to go one release
later and has outlived that by several; deleting it is outstanding work.

Two consequences worth keeping: the rule appears in `server ls` **only when some server carries one** — a
column that never varies is a column readers learn to skip — and `profile tool ls --all` names **which**
layer took each tool.

Two things that must not be re-simplified: the blocking layer is derived from the same `scope.Merge` as
the verdict, never from a second reading of the rules; and `inspect` stays **one** implementation, with
`profile tool inspect` being that report narrowed after it is computed, machine-wide verdict kept —
hiding it would claim a profile allows something no client can reach.
