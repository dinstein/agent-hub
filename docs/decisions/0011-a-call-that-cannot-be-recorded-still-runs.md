# 0011 — A `tools/call` that cannot be recorded still runs

> **Status** active · **Behaviour** [subsystems/records.md](../subsystems/records.md), [flows.md#the-call-ledger](../flows.md#the-call-ledger)

Every observability stream in the tree fails **open** — logs, events, the wire trace, and both tiers of
the call ledger. A write failure costs the history a line, is logged at Error as `ledger record dropped;
the call is unaffected`, and costs the call nothing.

The evidence tier used to refuse the governed method, on the rule that an unrecorded call is a governance
gap. Three things were wrong with it. **It protected nothing**: the record it was defending was already
lost at the moment the write failed, so refusing afterwards only added a second failure. **It put
availability in the wrong place**: a full disk or an unreadable vault stopped every tool a client had, and
this ledger has no permission role. **One of its three sites could not even be safe**: the finish is
written after the downstream has run, so replacing that response reported a failure that had not happened
and invited a client to repeat a side effect.

What did not change: metadata is still always on, evidence is still opt-in, recording still happens before
the gate chain, and the capacity, retention and free-space bounds are still hard — nothing is written past
them. **Fail-open is about the CALL, never about the bound.**
