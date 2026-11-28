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

## Package-local constraints

The extractor turns SSA into ordered sites before resolving their requirements:

- A return with interface data seeds a demand for its value. Producers of an
  interface value can know its concrete type even when the caller cannot.
- A reflected object name enables stores into that type and conversions from
  it. The effects demand the stored value or the type produced by the
  conversion.
- A call site maps required declared parameter positions to SSA values, and
  required type-parameter positions to the types substituted at the call. A
  `reflect.TypeFor` call also seeds a type demand directly.

Value demands still use the existing producer walks through closures, globals,
containers, and other SSA values. The package cache still merges named
requirements, reflected object facts, and forwarding edges from dependencies.
Local sites and closure identities are not serialized.

The local worklist indexes consumers by named API, closure, generic function,
or reflected object. A change wakes only the functions that use the fact. A
function still publishes its parameter requirements together, while names and
type-parameter requirements become visible within the function. Worklist visits
follow the old sweep order: a newly affected function later in the order can
run in the current pass, while one already visited waits for the next pass.

This order matters for unresolved calls. Signature-based requirements are a
fallback, used only if the call has no named requirements at the time it is
visited. Possible local closures can contribute requirements as well. A later
named fact does not retract names previously retained via the fallback. Treating
the signature as a second unconditional source, or processing the same facts
in an arbitrary order, could change which names are kept.

The `reflect` script checks the result across separately compiled packages,
including late interface matching and names that must remain obfuscated. The
local SSA tests also exercise backward chains, generic substitution, closures,
and the ordering of signature fallback and named facts.
