# Live block duplication experiment

This opt-in prototype explores selective duplication for #462:

```go
//garble:controlflow flatten_passes=0 block_duplicates=1
```

The parameter is a per-function budget from 0 to 4. A candidate is a shared return block with at least two predecessors, at most eight instructions, and only commutative integer binary operations before its return. Blocks with Phi instructions, calls, allocations, memory operations, or values used outside the block are excluded. The prototype clones the instruction objects, remaps their local dependencies, and redirects one seed-selected predecessor. Both copies remain reachable on real input paths.

The copy reverses the operands of its commutative operations. This is intentionally a narrow first experiment, not a general instruction-cloning implementation. It preserves side-effect counts by excluding side effects from the duplicated block. The omitted or zero parameter leaves normal behavior unchanged.

## Compiler probe

`TestExperimentMetrics` compiles a function with four conditionals, observable global updates, and a shared pure integer return block. Every executable checks its return values and accumulated side effects. Normal optimization is enabled and the measured function is not inlined. Seeds are 1, 2, and 3.

On Go 1.27.0, linux/amd64:

| Variant | Function bytes | Machine instructions | Return instructions |
| --- | --- | --- | --- |
| SSA round-trip without flattening, every seed | 100 | 23 | 1 |
| One duplicated block, every seed | 105 | 25 | 2 |

The compiler retains two live returns rather than merging the copies completely. However, all three transformed instruction sequences are identical after removing source locations and assembler metadata. Seed-selected predecessor choice and operand reversal did not produce compiled cross-seed diversity on this fixture. This is a negative result for that goal, not evidence that duplication generally defeats matching.

The probe also saves executable sizes and benchmark data. All benchmark samples reported zero allocations. Timings varied on the shared machine, including for identical baseline code, so they do not support a performance claim. No deobfuscator or function-matching evaluation has been performed.

```sh
CF_EXPERIMENT_DIRECTIVE='flatten_passes=0 block_duplicates=1' \
CF_EXPERIMENT_OUTPUT="$PWD/.local-duplication-evidence" \
go test ./internal/ctrlflow -run '^TestExperimentMetrics$' -v -count=1
```

Normal tests check that two return blocks are generated, both legitimate paths execute correctly, side effects occur exactly once, and same-seed output is reproducible. The existing `ctrlflow.txtar` fixture checks the public Garble path with literal obfuscation.

A stronger variant needs genuinely different computations in the copies, broader safe cloning rules, and a measured matching benefit under a strict growth budget. The existing SSA-to-AST map-iteration caveat remains unchanged.
