# stub_server.py — placeholder for the MCP 2026-07-28 Python cross-language
# validation server (docs/status/mcp-2026-07-28.md §4.3).
#
# Not implemented, and nothing runs this file. If it is ever built, it would
# use the mcp Python SDK beta (v2.0.0b1+) to serve a minimal 2026-07-28
# downstream that the Go suite connects to, checking the wire format against a
# second independent implementation rather than only against ourselves.
#
# Whoever builds it also has to add the target that runs it and the gate that
# keeps it out of `make ci`; neither exists. An earlier version of this comment
# said to run `TEST_PYSERVER=1 make e2e-integration`, which has never been a
# target in this tree.
