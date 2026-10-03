# Control-flow adversarial evaluation

Related to #462. This is an executed compiler comparison of the four original experiment heads, not a claim that any should become a default. Exact commits, compact instruction fingerprints, sizes, build times and matching totals are in `CONTROLFLOW_EVALUATION_RESULTS.json`.

The corpus has six functions: a sequence of live decisions, an alternating loop, a multi-Phi join, nested loops with break/continue, a switch, and an early return from a loop. All use uint32 arithmetic. Each executable compares transformed functions with untransformed references on case boundaries, non-cases and overflow inputs. Eight seeds produce 56 executables across seven modes. Every executable was run and all six measured functions were disassembled with normal compiler optimization and no inlining of their parent functions.

## Function matching

The baseline attack knows function boundaries but does not use function names as matching inputs. It compares opcode sequences with `difflib.SequenceMatcher`, ignores operands and addresses, and requires a unique best match among the six candidates. All 28 seed pairs are compared in one direction, making 168 function comparisons per mode. Ties count as failures.

Go 1.27.0 linux/amd64 results:

| Mode | Correct unique function matches | Total measured function bytes per binary |
| --- | --- | --- |
| Ordinary source | 168 / 168 | 269 |
| SSA round-trip, no pass | 168 / 168 | 274 |
| Existing full flattening | 168 / 168 | 1181 to 1199 |
| Original partial edge-group prototype | 147 / 168 | 419 to 469 |
| State-dependent transitions | 168 / 168 | 1348 to 1382 |
| Live integer substitution | 168 / 168 | 456 |
| Live block duplication | 168 / 168 | 282 |

This weak matcher identifies every function in six of the seven modes. Partial routing reduces its accuracy on this small corpus, but that is not enough to claim general resistance. The new connected-region follow-up is intentionally not substituted into this original-head comparison. Its separate report measures the changed implementation.

Basic blocks are split at direct branch targets and following instructions. The block metric in the JSON counts queries whose best-scoring candidate blocks belong exclusively to the correct function. It measures block-to-function ownership, not one-to-one original-block correspondence. Small repeated blocks create ties; changing block counts changes its denominator. Do not interpret it as recovered-edge accuracy.

## Simplification and graph recovery

The follow-up to the state-transition experiment includes `TestStatePropagationAttack`. With original-block boundaries supplied and bootstrap state zero, it follows Phi assignments, evaluates known XOR transitions, and prunes dispatcher comparisons. It does not read generator keys or annotations. Expected edges are recorded independently before transformation.

For six fixtures and eight seeds, that known-boundary SSA attack recovers all 344 original edge instances in both ordinary flattening and the canonical-key transition prototype. Unknown dynamic states fail closed. This removes the added state dependency under the stated assumptions; it is not a binary-only recovered-graph result.

The compiler matcher and SSA propagation attack are deliberately separate. No post-simplification binary matching score is supplied, and no blind graph-recovery percentage is invented.

## Cost and scope

`CONTROLFLOW_EVALUATION_BENCHMARKS.json` records three seed-1 benchmark samples for each of the six functions in all seven modes. All reported zero bytes and zero allocations per operation. Timings and warm/shared-cache build durations are retained as observations, not quiet-machine performance evidence. The compact report also records complete executable sizes, which include runtime overhead and should not be equated with summed function instruction bytes.

These probes invoke the real control-flow transform and then the normal Go compiler. They do not invoke Garble's whole-program naming/literal/runtime transformation. Ordinary source is therefore not an end-to-end ordinary-Garble baseline. The existing integration fixtures and upstream CI exercise those interactions separately. Blind binary matching, recovered edges after machine-code simplification, larger applications and quiet-machine runtime remain unmeasured.

## Reproduction

Fetch the original PR heads so their pinned objects are available. Python 3.12 or newer and an amd64 Go 1.27.0 toolchain are suitable for this recorded run.

```sh
git fetch https://github.com/luames/garble.git \
  explore/partial-dispatchers explore/state-transitions \
  explore/live-integers explore/block-duplication
python3 -m unittest discover -s scripts -p controlflow_evaluate_test.py -v
python3 scripts/controlflow_evaluate.py --output /absolute/new/evidence-directory
python3 scripts/controlflow_report.py /absolute/new/evidence-directory/results.json \
  --output /absolute/new/summary.json
python3 scripts/controlflow_benchmark.py /absolute/new/evidence-directory \
  --output /absolute/new/benchmarks.json
```

The evaluator exports each pinned commit into an isolated directory, generates source, compiles, runs and disassembles it. It rejects a reused output directory and fails if an external command or runtime check fails. Keep raw evidence outside Git; the checked-in JSON is a compact report generated from actual outputs. The matching/parser unit tests run without compilers or network calls.
