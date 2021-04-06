## Contributing to Garble

Thank you for your interest in contributing! Here are some ground rules:

1. The tool's design decisions are in the [README](README.md)
2. New features or major changes should be opened as an issue first
3. All contributions are done in PRs with at least one review and CI
4. All changes that alter behavior (features, flags, bugs) need a test
5. We use the `#obfuscation` channel over at the [Gophers Slack](https://invite.slack.golangbridge.org/) to chat

When contributing for the first time, you should also add yourself to the
[AUTHORS file](AUTHORS).

### Testing

Just the usual `go test ./...`; many of the tests are in
[testscript](https://godoc.org/github.com/rogpeppe/go-internal/testscript) under
`testdata/script/`, which allows laying out files and shell-like steps to run as
part of the test.

Note that the tests do real builds, so they are quite slow; on an average
laptop, `go test` can take over thirty seconds. Here are some tips:

* Use `go test -short` to skip some extra and slow sanity checks
* Use `go test -run Script/foo` to just run `testdata/scripts/foo.txt`

### Development tips

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
