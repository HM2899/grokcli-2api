# Compatibility contracts

This directory is the language-neutral boundary between the current Python
runtime (the behavioral oracle) and the incremental Go replacement.

Rules:

1. Fixtures describe externally observable behavior, not Python internals.
2. Timestamps, generated IDs, and account IDs may be normalized only where the
   manifest explicitly marks them nondeterministic.
3. SSE comparisons preserve event order, terminal events, and `[DONE]`.
4. Redis key formats/TTLs and PostgreSQL null-vs-absent semantics are contracts
   whenever Python and Go coexist.
5. Any intentional behavior change requires a reviewed fixture/manifest change
   before either implementation is changed.

`manifest.json` starts with the highest-risk stable surfaces. Fixture corpora
and generated OpenAPI snapshots are added incrementally as the deterministic
fake upstream covers each scenario.
