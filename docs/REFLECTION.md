# Reflection dataflow

Garble analyzes one Go package per compile action. A dependency's SSA is not
available when a downstream package learns that a call in that dependency can
reach reflection. The analysis therefore separates facts known while compiling
one package from relationships that must survive in its cache.

## Cached parameter constraints

A fact `requires(f, i)` means calls to `f` may expose declared argument `i` to
reflection. Indices exclude the receiver and synthetic SSA parameters. A
`reflectCallEdge` records an implication extracted from SSA: if the callee's
parameter requires reflection, so does the caller parameter forwarded to it.
The edge is retained even when no reflective target is known yet.

After dependency summaries are merged and (for the main package) concrete
methods are matched to reachable interfaces, `solveReflectCallEdges` computes
the least set containing the known `ReflectAPIs` facts and closed under those
implications. It wakes the edges whose callee facts became known. This makes the
result independent of edge order: mutually recursive functions need a seed to
contribute a new fact, and different positions of a function are independent.
The solver needs neither SSA nor a new cache format.

## Extending the model

The package-local analysis still iterates over SSA. A next step is to extract
constraints for local facts first, without changing which names are retained:

- For an argument required by a callee, trace the value to its producers and
  to the enclosing function's declared parameters. Closures and allocations
  need local identities; those are not suitable cache keys. Global stores and
  closure bindings already have producer indexes that can feed this step.
- Keep reflected type arguments and reflected objects as separate kinds of
  facts. A generic call substitutes type arguments; for a store or conversion,
  the requirement comes from its reflected type rather than a callee position.
- Treat ambiguous calls as a dispatch problem, not a single edge. Today a
  dynamic call can fall back to requirements keyed by signature when there are
  no requirements for its name, and can combine requirements from several
  local closures. Unconditionally treating the signature as another source is
  not equivalent: it can retain extra names once exact requirements are known.
  Keep this policy separate until a change in precision can be measured.

For each migration, compare the complete requirement sets and the names
retained by old and new analysis, including negative cases where unrelated
names must stay obfuscated. The `reflect` script exercises late interface
matching across separately compiled packages; solver tests alone cannot verify
that the SSA extractor or package cache supplied the right edges.
