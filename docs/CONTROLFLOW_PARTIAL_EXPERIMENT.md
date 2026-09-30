# Partial dispatcher experiment

This is an opt-in experiment for #462, not a proposal to enable control flow obfuscation by default.

```go
//garble:controlflow flatten_passes=1 flatten_regions=2
```

The prototype selects half of the eligible edges, up to 32, and distributes them between two independent dispatchers. An edge is ineligible if its target can reach its source. This conservatively leaves every cycle edge direct. At least eight eligible edges are required; smaller functions are left unflattened. Ordinary flattening is unchanged when `flatten_regions` is omitted or zero. Other pass counts are rejected in this mode.

These are two seed-selected edge groups, not connected single-entry regions. The experiment answers whether partial routing and multiple state variables survive Go's optimizer. It does not yet implement profile-guided region selection. Original Phi assignments remain at the original predecessors before routing through adapters, matching the existing converter's lowering contract.

## Compiler probe

The probe in `TestExperimentMetrics` uses a real function with four conditionals, observable global updates, and a live integer return. Each generated program verifies its return values and the accumulated side effects before benchmarking. It compiles with normal Go optimization and disables inlining of the measured function. Seeds are 1, 2, and 3.

Go 1.27.0, linux/amd64 results:

| Variant | Function bytes, seeds 1/2/3 | Machine branch instructions, seeds 1/2/3 |
| --- | --- | --- |
| SSA round-trip without flattening | 100 / 100 / 100 | 8 / 8 / 8 |
| Existing full flattening | 364 / 351 / 368 | 36 / 36 / 36 |
| Partial, two dispatchers | 196 / 195 / 194 | 17 / 19 / 18 |

The three partial builds have different opcode sequences. This confirms compiled structural variation on this fixture, not resistance to function matching or control-flow recovery. The full-flattening builds also differ across seeds. No deobfuscator comparison has been performed.

The probe recorded runtime and allocations too. Other builds were running on the same machine, and timings varied substantially even for identical baseline instruction sequences. They are not evidence of a speedup. Each benchmark reported zero allocations.

Reproduce the raw source, assembly, executable, and benchmark logs with:

```sh
CF_EXPERIMENT_DIRECTIVE='flatten_passes=1 flatten_regions=2' \
CF_EXPERIMENT_OUTPUT="$PWD/.local-partial-evidence" \
go test ./internal/ctrlflow -run '^TestExperimentMetrics$' -v -count=1
```

Keep evidence outside version control. The normal tests check two dispatchers, same-seed reproducibility, and compiled execution over multiple input paths. The existing `ctrlflow.txtar` fixture also exercises the parameter through Garble with literal obfuscation.

Before promotion, evaluate connected regions, larger real programs, loop handling, and recovered control flow rather than branch counts alone. The existing map-iteration caveat in `CONTROLFLOW.md` remains unchanged.
