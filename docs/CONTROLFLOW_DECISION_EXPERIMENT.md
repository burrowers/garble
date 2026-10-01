# Multi-way decision lowering experiment

Related to #462. This prototype is opt-in:

```go
//garble:controlflow flatten_passes=0 decision_lowering=1
```

It recognizes an entry chain of three to sixteen equality comparisons of the same integer parameter against distinct constant values. Each comparison block must contain only the comparison and its branch. The false chain ends at a returning default block. Functions with Phis, calls, allocations or defers are excluded. Tag expressions and observable case-expression evaluations are not moved; they fail eligibility. Phi-bearing joins and nontrivial fallthrough paths are excluded by these constraints.

A seed chooses either a permuted equality chain or a balanced arrangement with grouping comparisons. The real cases and default remain live. Each retained equality still targets the same case body; grouping comparisons route the remaining decisions. Parameters disabled or omitted consume no additional randomness. Other structural passes and hardening are rejected in this first experiment.

## Optimized compiler result

The probe switches on a uint32 parameter with cases 0, 1, 2, 6 and 17 and a default arithmetic result. Generated code is compiled with normal optimization and the measured function is not inlined. Executables check case boundaries, non-cases, and overflow inputs.

Across eight fixed seeds on Go 1.27.0 linux/amd64, the compiled function occupies 58 or 59 instruction bytes and contains five or six branch instructions. Several seed choices normalize to the same compiled form; the grouped form adds a branch on this fixture. The compiler therefore preserves limited variation, not eight distinct compiled decisions. This is not evidence of increased resistance to matching or graph recovery.

```sh
CF_DECISION_OUTPUT="$PWD/.local-decision-evidence" \
go test ./internal/ctrlflow -run '^TestDecisionCompilerProbe$' -count=1
```

Focused tests cover deterministic source, multiple seeds and input paths, disabled mode and rejected nonconstant/fallthrough/join cases. `go test ./internal/ctrlflow ./internal/ssa2ast -count=1` and `go vet ./internal/ctrlflow ./internal/ssa2ast` passed locally. The existing Garble integration script includes a runtime check of this mode with literal obfuscation.

This remains a draft research rule. Runtime and allocation comparison on a quiet machine is not provided. Map iteration is outside the recognized entry-chain form; the source converter's existing caveat remains unchanged.
