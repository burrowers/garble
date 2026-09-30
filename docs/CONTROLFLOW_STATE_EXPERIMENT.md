# State-dependent transition experiment

This opt-in prototype explores predecessor-dependent transitions for #462:

```go
//garble:controlflow flatten_passes=1 state_transitions=1
```

Instead of writing a target state constant, each adapter computes `currentState ^ edgeDelta`. Each original destination block has one seed-generated state key. The delta is the XOR of the source block's key and the target block's key. Every incoming path therefore establishes the same state for a block, including join blocks and loop headers. Zero remains the bootstrap key of the original entry block.

The dispatcher Phi carries the state into the transition instructions. The new values explicitly refer back to the Phi so the converter does not discard them as unused or give them incorrect local scope. When disabled, the existing per-edge constants and random stream remain unchanged.

This mode requires exactly one flattening pass and rejects nonzero block splitting, junk jumps, trash blocks, or `flatten_hardening`. Those hardeners remap compare/store values after SSA construction and would invalidate the source-key invariant. Splitting a block containing multiple Phi instructions can also violate the converter's predecessor contract, so composition is deliberately excluded from this first experiment. It is not an opaque predicate and is reversible by propagating state constants or tracing execution.

## Compiler probe

`TestExperimentMetrics` compiles a function with four real conditionals, observable global updates, and a live integer return. Every generated executable checks return values and accumulated side effects. Normal optimization is enabled and the measured function is not inlined. Seeds are 1, 2, and 3.

Go 1.27.0, linux/amd64 results:

| Variant | Function bytes, seeds 1/2/3 | Machine instructions, seeds 1/2/3 |
| --- | --- | --- |
| Existing full flattening | 364 / 351 / 368 | 93 / 90 / 93 |
| State-dependent transitions | 376 / 401 / 401 | 79 / 85 / 85 |

The transition calculations survive optimization. The resulting instruction sequences differ across seeds, although seeds 2 and 3 have the same opcode-only sequence. Sharing destination keys lets the compiler eliminate some repeated dispatch comparisons, so a lower instruction count does not mean stronger obfuscation. No recovered-control-flow or function-matching experiment has been performed.

Runtime and executable-size data are also emitted by the probe. All benchmark samples reported zero allocations. Shared-machine runtime noise prevents a defensible slowdown estimate from these samples.

```sh
CF_EXPERIMENT_DIRECTIVE='flatten_passes=1 state_transitions=1' \
CF_EXPERIMENT_OUTPUT="$PWD/.local-state-evidence" \
go test ./internal/ctrlflow -run '^TestExperimentMetrics$' -v -count=1
```

Normal tests execute loops and joins over several inputs and seeds and check same-seed reproducibility. The existing `ctrlflow.txtar` fixture exercises the mode through Garble with literal obfuscation.

Before promotion, test a larger corpus, state propagation attacks, hardening composition, nested flattening, and cost on a quiet machine. The existing SSA-to-AST map-iteration caveat remains unchanged.
