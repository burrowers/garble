# Live-operation outlining experiment

Related to #462. This prototype is opt-in:

```go
//garble:controlflow flatten_passes=0 outline_ops=1 outline_noinline=1
```

`outline_ops=1` moves one seed-selected live binary operation into a two-argument helper. It accepts only a single straight-line SSA block, with parameters and one result of the same built-in uint8, uint16, uint32 or uint64 type. Supported operations are addition, subtraction, multiplication, XOR, AND and OR. Calls, memory operations, branches, loops, closures, methods, defer/recover and shifts are excluded. This deliberately small experiment does not outline arbitrary regions or escaping state.

`outline_noinline` defaults to zero. When set to one, the helper receives `//go:noinline`; it requires `outline_ops=1`. This experiment requires `flatten_passes=0` and rejects all other structural or hardening passes. Omitting or disabling both parameters preserves source generation and the existing random stream.

## Optimized compiler result

The probe computes `((n+7)^0x13579)*3-n` with uint32 wraparound. The measured parent function is not inlined. Eight fixed seeds select different live operations. All generated executables are checked, and normal Go optimization is enabled.

Go 1.27.0 linux/amd64 results:

| Variant | Parent plus retained helper instruction bytes | Retained helper |
| --- | --- | --- |
| SSA round-trip baseline | 17 for every seed | No |
| Outlining, compiler may inline helper | 18 or 19 | No |
| Outlining, noinline helper | 64, 66 or 70 | Yes |

The inlined variants retain small instruction differences, but the requested function-boundary change disappears. Preventing inlining preserves one helper call and changes which operation crosses the call boundary. It also adds stack/call machinery and substantially increases measured code bytes on this tiny fixture. This is a rejection criterion for automatic use of this particular rule, not evidence of stronger matching resistance. No stable call-overhead or quiet-machine runtime claim is made.

```sh
CF_OUTLINE_OUTPUT="$PWD/.local-outline-evidence" \
go test ./internal/ctrlflow -run '^TestOutliningCompilerProbe$' -count=1
```

Normal tests cover wraparound inputs, eight seeds, determinism, disabled mode, and exclusions. `go test ./internal/ctrlflow ./internal/ssa2ast -count=1` and `go vet ./internal/ctrlflow ./internal/ssa2ast` passed locally. The existing `TestScript/ctrlflow` fixture also passed through Garble with literal and name obfuscation enabled.

Keep this as a draft experiment. A larger safe region might amortize call overhead, but that is not implemented or measured here. Map iteration is outside the eligibility rules; the existing converter caveat is unchanged.
