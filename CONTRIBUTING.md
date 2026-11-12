## Contributing to Garble

Thank you for your interest in contributing! Here are some ground rules:

1. The tool's design decisions are in the [README](README.md)
2. Always file an issue before sending a PR: a bug report for a fix, or a
   feature request for anything new. Discussing the problem first is the best
   way to agree on whether and how it should be solved, so that your work is
   not wasted. PRs without an issue may be closed in favor of one, with the
   exception of trivial fixes such as typos.
3. All contributions are done in PRs with at least one review and CI
4. All changes that alter behavior (features, flags, bugs) need a test
5. We use the `#obfuscation` channel over at the [Gophers Slack](https://invite.slack.golangbridge.org/) to chat

### Testing

Just the usual `go test ./...`; many of the tests are in
[testscript](https://pkg.go.dev/github.com/rogpeppe/go-internal/testscript) under
`testdata/scripts/`, which allows laying out files and shell-like steps to run as
part of the test.

For editor support of testscript files in vscode (highlighting and formatting), install the
[vscode-testscript](https://marketplace.visualstudio.com/items?itemName=twpayne.vscode-testscript) extension.

Note that the tests do real builds, so they are quite slow; on an average
laptop, `go test` can take over thirty seconds. Here are some tips:

* Use `go test -short` to skip the more expensive or thorough tests
* Use `go test -run Script/foo` to just run `testdata/scripts/foo.txt`

### Integrating into Go builds

When you run `go build`, it first loads all needed packages.
It then figures out how to build them in distinct steps,
such as the calls to `asm`, `compile`, or `link` you can see in `go build -x`.
Note how each of these toolchain tools is executed in separate processes.
For example, a small `go build` might look like the following process tree:

	go build ./main
		↳ …/compile -p project.com/library library.go
		↳ …/compile -p main main.go
		↳ …/link -o main.exe

`garble build` boils down to calling `go build -toolexec=garble`,
which is a flag that lets us wrap the calls to each tool mentioned above.
For example, where a regular build might run `…/compile foo.go`,
`-toolexec=garble` instead runs `garble …/compile foo.go`,
which lets us obfuscate the Go source code before being compiled.

Because of the above, `garble` gets run as multiple processes:
one top-level process that implements the CLI and the initial setup,
and one process for each compilation step tool that we transform.
For example, here's a `garble build` version of the earlier process tree:

	garble build ./main
		↳ go build -toolexec=garble ./main
			↳ garble …/compile -p project.com/library library.go
				↳ …/compile -p project.com/library library.go
			↳ garble …/compile -p main main.go
				↳ …/compile -p main main.go
			↳ garble …/link -o main.exe
				↳ …/link -o main.exe

Go builds happen one package at a time, and so does garble's obfuscation.
This is necessary to support build caching and incremental builds.
For further build speed, packages are built in parallel whenever possible,
with each package only building once all of its dependencies are finished.

To deduplicate work, the top-level garble process loads all packages to build,
and stores their information in a file consumed by the garble sub-processes.
Each garble sub-process also produces extra cached output of its own,
with information such as which declared names could not be obfuscated.
garble sub-processes will load the cached output files for their dependencies.

### Development tips

To see how garble is obfuscating a build, you can use `garble -debug build`.
You can also use `-debugdir` to get a copy of the obfuscated source code.
To get finer-grained information, adding temporary debug prints is helpful.

When investigating an issue, such as a build failure or subpar obfuscation,
it's best to reproduce the problem with the smallest build input possible.
That will help write a test case, and also make `garble -debug` more useful.
For example, if you suspect what piece of code might be causing the issue,
try moving the code to one or two new packages with very few dependencies.

To inject code into the syntax tree, don't write `go/ast` nodes by hand; you can
generate them by typing Go source into tools such as
[astextract](https://lu4p.github.io/astextract/).

### Terminology

The *Go toolchain*, or simply *the toolchain*, refers to the `go` command and
all of its components used to build programs, such as the compiler and linker.

An *object file* or *archive file* contains the output of compiling a Go
package, later used to link a binary.

An *import config* is a temporary text file passed to the compiler via the
`-importcfg` flag, which contains an *object file* path for each direct
dependency.

A *build ID* is a slash-separated list of hashes for a build operation, such as
compiling a package or linking binary. The first component is the *action ID*,
the hash of the operation's inputs, and the last component is the *content ID*,
the hash of the operation's output. For more, read
[the docs in buildid.go](https://github.com/golang/go/blob/master/src/cmd/go/internal/work/buildid.go)

### PR cost reports

GitHub Actions maintains one informational comment per PR with two independent
sections. Increases never fail a check; failed builds or tests still fail their
usual jobs.

* **Build cost:** the same HTTP/JSON executable is built with the base and head
  Garble binaries on one ephemeral Linux runner, in default, `-literals`, and
  `-tiny -literals` modes. Each mode uses three samples per revision, a fixed seed,
  and alternating revision order. Every sample starts with empty Go and Garble
  caches, so elapsed time includes patched-tool preparation but not building
  Garble itself. The report shows executable bytes, elapsed medians and sample
  ranges. Each executable is run and its output checked. This is one workload
  and seed, not a statistical regression test or an application runtime benchmark.
* **Test cost:** the existing full Linux test job records elapsed time and OS
  user/system CPU seconds for `go test -count=1 -timeout=15m ./...`, including
  waited-for descendants and the builds performed by integration tests. It does
  not profile individual tests, serialize them, reset their build caches, or run
  another suite. A delta requires an artifact from the exact base commit's
  `master` push, matching Go patch version, CPU model, CPU count, runner profile,
  and command. Ordinary CI cache state is uncontrolled, so even a compatible
  comparison remains a noisy signal. Failed suites and missing/incompatible
  baselines are reported without a delta.

Raw measurements are retained as Actions artifacts for 14 days. An expired or
not-yet-recorded base is not replaced with an unrelated commit. The test section
covers the full Linux suite, not the race/386/third-party jobs or the other OSes.

The build-metrics job runs fork code without comment-write credentials on an
ephemeral runner, without persistent caches. The existing Test jobs retain their
usual Namespace caches. A separate `workflow_run` workflow checks out only the
default branch, validates
bounded numeric JSON without extracting or executing artifact files, binds the
measurements to the PR's current base/head and source repository, and updates the
comment. PR code can falsify its own numeric artifact: these are review aids, not
trusted attestations. Superseded-head sections are discarded rather than reused.

The build-metrics job uses the base revision's collector and workload so that a
PR cannot accidentally change the comparison inputs. Only the initial PR adding
this infrastructure bootstraps its proposed harness, with a notice in the log.
The automatic publisher becomes active once its workflow is on the default
branch. Likewise, test deltas need a successful instrumented base test run; the
first PR introducing this infrastructure has no historical test-cost baseline.

Run the reporting-tool tests with:

    python3 -B -m unittest discover -s scripts -p 'test_*.py'

### Benchmarking

A build benchmark is available, to be able to measure the cost of builing a
fairly simple main program with and without caching. Here is an example of how
to use the benchmark with [benchstat](https://golang.org/x/perf/cmd/benchstat):

	# Run the benchmark six times with five iterations each.
	go test -run=- -bench=. -count=6 -benchtime=5x >old.txt

	# Make some change to the code.
	git checkout some-optimization

	# Obtain benchmark results once more.
	go test -run=- -bench=. -count=6 -benchtime=5x >new.txt

	# Obtain the final stats.
	benchstat old.txt new.txt

It is very important to run the steps above on a quiet machine. Any background
program that could use CPU or I/O should be closed, as it would likely skew the
results; this includes browsers, chat apps, and music players.

A higher `-benchtime` will mean more stable numbers, and a higher `-count` will
mean more reliable statistical results, but both increase the overall cost of
running the benchmark. The provided example should be a sane default, and each
'go test' invocation takes about a minute on a laptop.

For example, below are the final results for a run where nothing was changed:

	name             old time/op       new time/op       delta
	Build/Cache-8          165ms ± 3%        165ms ± 2%   ~     (p=1.000 n=6+6)
	Build/NoCache-8        1.26s ± 7%        1.27s ± 5%   ~     (p=0.699 n=6+6)

	name             old bin-B         new bin-B         delta
	Build/Cache-8          6.36M ± 0%        6.36M ± 0%   ~     (all equal)
	Build/NoCache-8        6.36M ± 0%        6.36M ± 0%   ~     (all equal)

	name             old sys-time/op   new sys-time/op   delta
	Build/Cache-8          205ms ± 6%        214ms ± 4%   ~     (p=0.093 n=6+6)
	Build/NoCache-8        512ms ± 6%        512ms ±12%   ~     (p=0.699 n=6+6)

	name             old user-time/op  new user-time/op  delta
	Build/Cache-8          829ms ± 1%        822ms ± 1%   ~     (p=0.177 n=6+5)
	Build/NoCache-8        8.44s ± 7%        8.55s ± 5%   ~     (p=0.589 n=6+6)
