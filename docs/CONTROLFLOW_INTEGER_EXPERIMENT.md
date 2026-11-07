# Live integer experiment

This opt-in prototype explores live data-flow substitutions for #462. It leaves existing behavior unchanged unless the directive enables it:

```go
//garble:controlflow flatten_passes=0 live_integers=1
```

For fixed-width unsigned additions, it substitutes `x+y` with `(x^y)+((x&y)<<1)`. The identity uses modular arithmetic at the original width, including overflow. Operands are already SSA values, so calls and other side effects are not evaluated twice. It retains the original result object, introduces typed intermediate instructions, and rebuilds referrers before conversion. Named types with an unsigned fixed-width underlying type are supported.

Signed and machine-width integers, floats, pointers, division, and original shift operations are excluded. This prototype inserts one fixed shift as part of the proven addition identity. It does not encode values across several operations. Operand order varies with the seed, but there is only one substitution rule.

## Findings

`TestExperimentMetrics` compiles and executes a function with four real conditionals, observable global updates, and an unsigned integer return. Normal Go optimization is enabled; the measured function is not inlined. Seeds are 1, 2, and 3. On Go 1.27.0, linux/amd64:

| Variant | Function bytes | Machine instructions |
| --- | --- | --- |
| SSA round-trip without flattening, every seed | 100 | 23 |
| Live substitution, every seed | 108 | 27 |

The substitution survives optimization, but all three transformed builds have identical instruction sequences after removing source locations and assembler metadata. Operand swapping alone does not provide compiled cross-seed diversity on this fixture. That is a negative result for the original diversity goal, even though the live computation changes compared with the baseline. This rule is recognizable and reversible; no deobfuscation-resistance claim follows from four additional instructions.

The probe also records executable sizes and benchmarks. All benchmark samples reported zero allocations. Runtime samples varied substantially on the shared machine, including for identical baseline code, so they do not support a performance claim. No function-matcher or deobfuscator evaluation has been performed.

Reproduce the raw generated source, optimized assembly, executable, and benchmark output:

```sh
CF_EXPERIMENT_DIRECTIVE='flatten_passes=0 live_integers=1' \
CF_EXPERIMENT_OUTPUT="$PWD/.local-integer-evidence" \
go test ./internal/ctrlflow -run '^TestExperimentMetrics$' -v -count=1
```

Normal tests exhaust all uint8 input pairs, cover uint16, uint64 and a named uint32 type at overflow boundaries, check excluded operations, and verify same-seed source reproducibility. The existing `ctrlflow.txtar` fixture checks uint64 overflow through Garble with literal obfuscation.

Keep this experimental. A useful next experiment needs several genuinely different surviving rules or persistent encoded values, plus normalization and matching measurements. The existing map-iteration caveat of the SSA-to-AST path remains unchanged.
