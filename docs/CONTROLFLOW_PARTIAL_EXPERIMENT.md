# Partial dispatcher experiment

This is an opt-in experiment for #462, not a proposal to enable control flow obfuscation by default.

```go
//garble:controlflow flatten_passes=1 flatten_regions=2
```

The prototype selects two disjoint, connected single-entry regions of three or four blocks. A new block joins a region only when all of its predecessors are already inside it. Blocks on any cycle are excluded, so loop-internal edges stay direct. Each region has its own dispatcher; entry, exit and all other edges remain direct. If no pair of bounded regions exists, the function is left unflattened. Ordinary flattening is unchanged when `flatten_regions` is omitted or zero. Other pass counts are rejected in this mode.

Selection is conservative and seed-dependent, not profile-guided. It replaces the earlier arbitrary edge groups. Original Phi assignments remain at the original predecessors before routing through adapters, matching the existing converter's lowering contract.

## Compiler probe

The probe in `TestExperimentMetrics` uses a real function with four conditionals, observable global updates, and a live integer return. Each generated program verifies its return values and the accumulated side effects before benchmarking. It compiles with normal Go optimization and disables inlining of the measured function. Seeds are 1, 2, and 3.

Go 1.27.0, linux/amd64 results:

| Variant | Function bytes, seeds 1/2/3 | Machine branch instructions, seeds 1/2/3 |
| --- | --- | --- |
| SSA round-trip without flattening | 100 / 100 / 100 | 8 / 8 / 8 |
| Existing full flattening | 364 / 351 / 368 | 36 / 36 / 36 |
| Connected partial, two dispatchers | 133 / 133 / 161 | 8 / 8 / 13 |

The three partial builds have different opcode sequences. This confirms compiled structural variation on this fixture, not resistance to function matching or control-flow recovery. The full-flattening builds also differ across seeds. No deobfuscator comparison has been performed.

The probe recorded runtime and allocations too. Other builds were running on the same machine, and timings varied substantially even for identical baseline instruction sequences. They are not evidence of a speedup. Each benchmark reported zero allocations.

Reproduce the raw source, assembly, executable, and benchmark logs with:

```sh
CF_EXPERIMENT_DIRECTIVE='flatten_passes=1 flatten_regions=2' \
CF_EXPERIMENT_OUTPUT="$PWD/.local-partial-evidence" \
go test ./internal/ctrlflow -run '^TestExperimentMetrics$' -v -count=1
```

Keep evidence outside version control. The normal tests check two dispatchers, same-seed reproducibility, and compiled execution over multiple input paths. The existing `ctrlflow.txtar` fixture also exercises the parameter through Garble with literal obfuscation.

Tests also check region connectivity, single-entry topology, disjointness, unchanged cycle edges, and compiled multi-Phi/loop execution over eight fixed seeds. Before promotion, evaluate larger real programs and recovered control flow rather than branch counts alone. The existing map-iteration caveat in `CONTROLFLOW.md` remains unchanged. These fixtures do not use map iteration.
