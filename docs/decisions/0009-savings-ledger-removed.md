# 0009 — The token-savings ledger was removed, not repaired

> **Status** active · **Behaviour** nothing; `internal/savings` and `agenthub activity` are gone

Its one writer fired only when a result budget cut a result, and the budget has no built-in default, so
on an untouched install nothing was ever written.

Three reasons not to repair it. **The unit was wrong** — four bytes per token is ±20% on English JSON and
understates CJK, least accurate for the payloads most worth shrinking. **The claim was wrong** — it
measured how much a configured budget truncated, a consequence of the operator's own setting. **The
absence was worse than silence** — 0 because nothing was recorded is indistinguishable from 0 because it
is broken.

**What must not be reintroduced under another name: an estimator with a fixed bytes-per-token divisor.**
Per-mechanism accounting, if ever wanted again, measures **bytes** — a fact this process observes, not a
third party's tokenizer. The TOON savings threshold is not an exception: it compares byte lengths to
decide whether the encoding is used at all.
