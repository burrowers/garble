## Contributing to Garble

Thank you for your interest in contributing! Here are some ground rules:

1. The tool's design decisions are in the [README](README.md)
2. New features or major changes should be opened as an issue first
3. Non-trivial contributions should be done in PRs with code review and CI
4. We use the `#obfuscation` channel over at the [Gophers Slack](https://invite.slack.golangbridge.org/) to chat

### Testing

Just the usual `go test ./...`; many of the tests are in
[testscript](https://godoc.org/github.com/rogpeppe/go-internal/testscript) under
`testdata/script/`, which allows laying out files and shell-like steps to run as
part of the test.

Note that the tests do real builds, so they are quite slow; on an average
laptop, `go test` can take over thirty seconds. Here are some tips:

* Use `go test -short` to skip some extra sanity checks
* Use `go test -run Script/foo` to just run `testdata/scripts/foo.txt`

### Development tips

To inject code into the syntax tree, don't write `go/ast` nodes by hand; you can
generate them by typing Go source into tools such as
[astextract](https://lu4p.github.io/astextract/).

### Benchmarking

A build benchmark is available, to be able to measure the cost of builing a
fairly simple main program. Here is an example of how to use the benchmark with
[benchstat](https://golang.org/x/perf/cmd/benchstat):

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

	$ benchstat old.txt new.txt
	name     old time/op       new time/op       delta
	Build-8        1.63s ± 6%        1.65s ± 6%   ~     (p=0.699 n=6+6)

	name     old sys-time/op   new sys-time/op   delta
	Build-8        1.18s ± 6%        1.22s ± 8%   ~     (p=0.310 n=6+6)

	name     old user-time/op  new user-time/op  delta
	Build-8        9.82s ± 6%       10.01s ± 7%   ~     (p=0.485 n=6+6)
