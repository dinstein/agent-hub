# 0004 — TOON is a one-way projection, and both grammars are frozen

> **Status** active · **Behaviour** [subsystems/shaping.md](../subsystems/shaping.md), [subsystems/exposure.md](../subsystems/exposure.md)

Determinism is the contract, so both grammars are frozen with golden corpora.

**(a) TOON does not round-trip and no decoder is provided.** A round trip would need a type marker on
every scalar — a bare `1` against `"1"` — which is exactly the tokens the encoding exists to save. The
contract is stated in-band by a first line reading `#toon/1 (display encoding; send tool arguments as
JSON)`, and anything that must survive a round trip — `structuredContent`, tool arguments, cursors —
stays JSON. Two constructive guarantees: **never larger** (below the savings threshold the input is
returned unchanged) and **numeric fidelity** (no value is routed through a float64).

**(b) The compact signature grammar** — `name(p1:str, p2?:int=3, p3?~:obj{a,b}) -> str`, `?` for optional
and `~` for lossy — is what a search hit returns *instead of* a schema.

Two ordering invariants are not local to those packages: `ShapeResult` **re-encodes first and applies the
budget second**, so the truncation trailer is always last, and re-encoding sits at the end of the delivery
path so nothing downstream can invalidate the budget that trailer describes.

Two-stage describe is part of the same ruling: the meta-tools are **five, in a frozen order**;
`describe_tool`'s visibility predicate is exactly the surface's own map, so it cannot be wider than
search, list or call; and it emits **one per-id error only**, because differentiated errors would make it
an enumeration oracle. The same rule governs `fetch_result`.
