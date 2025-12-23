# garble

	go install mvdan.cc/garble@latest

Obfuscate Go code by wrapping the Go toolchain. Requires Go 1.25 or later.

	garble build [build flags] [packages]

The tool also supports `garble test` to run tests with obfuscated code,
`garble run` to obfuscate and execute simple programs,
and `garble reverse` to de-obfuscate text such as stack traces.
Run `garble -h` to see all available commands and flags.

You can also use `go install mvdan.cc/garble@master` to install the latest development version.

### Purpose

Produce a binary that works as well as a regular build, but that has as little
information about the original source code as possible.

The tool is designed to be:

* Coupled with `cmd/go`, to support modules and build caching
* Deterministic and reproducible, given the same initial source code
* Reversible given the original source, to de-obfuscate panic stack traces

### Mechanism

The tool wraps calls to the Go compiler and linker to transform the Go build, in
order to:

* Replace as many useful identifiers as possible with short base64 hashes
* Replace package paths with short base64 hashes
* Replace filenames and position information with short base64 hashes
* Remove all [build](https://go.dev/pkg/runtime/#Version) and [module](https://go.dev/pkg/runtime/debug/#ReadBuildInfo) information
* Strip debugging information and symbol tables via `-ldflags="-w -s"`
* [Obfuscate literals](#literal-obfuscation), if the `-literals` flag is given
* Remove [extra information](#tiny-mode), if the `-tiny` flag is given

By default, the tool obfuscates all the packages being built.
You can manually specify which packages to obfuscate via `GOGARBLE`,
a comma-separated list of glob patterns matching package path prefixes.
This format is borrowed from `GOPRIVATE`; see `go help private`.

Note that commands like `garble build` will use the `go` version found in your
`$PATH`. To use different versions of Go, you can
[install them](https://go.dev/doc/manage-install#installing-multiple)
and set up `$PATH` with them. For example, for Go 1.17.1:

```sh
$ go install golang.org/dl/go1.17.1@latest
$ go1.17.1 download
$ PATH=$(go1.17.1 env GOROOT)/bin:${PATH} garble build
```

### Use cases

A common question is why a code obfuscator is needed for Go, a compiled language.
Go binaries include a surprising amount of information about the original source;
even with debug information and symbol tables stripped, many names and positions
remain in place for the sake of traces, reflection, and debugging.

Some use cases for Go require sharing a Go binary with the end user.
If the source code for the binary is private or requires a purchase,
its obfuscation can help discourage reverse engineering.

A similar use case is a Go library whose source is private or purchased.
Since Go libraries cannot be imported in binary form, and Go plugins
[have their shortcomings](https://github.com/golang/go/issues/19282),
sharing obfuscated source code becomes an option.
See [#369](https://github.com/burrowers/garble/issues/369).

Obfuscation can also help with aspects entirely unrelated to licensing.
For example, the `-tiny` flag can make binaries 15% smaller,
similar to the [common practice in Android](https://developer.android.com/build/shrink-code#obfuscate) to reduce app sizes.
Obfuscation has also helped some open source developers work around
anti-virus scans incorrectly treating Go binaries as malware.

### Literal obfuscation

Using the `-literals` flag causes literal expressions such as strings to be
replaced with more complex expressions, resolving to the same value at run-time.
String literals injected via `-ldflags=-X` are also replaced by this flag.
This feature is opt-in, as it can cause slow-downs depending on the input code.

Literals used in constant expressions cannot be obfuscated, since they are
resolved at compile time. This includes any expressions part of a `const`
declaration, for example.

Note that this process can be reversed given enough effort;
see [#984](https://github.com/burrowers/garble/issues/984).

### Tiny mode

With the `-tiny` flag, even more information is stripped from the Go binary.
Position information is removed entirely, rather than being obfuscated.
Runtime code which prints panics, fatal errors, and trace/debug info is removed.
Many symbol names are also omitted from binary sections at link time.
All in all, this can make binaries about 15% smaller.

With this flag, no panics or fatal runtime errors will ever be printed, but they
can still be handled internally with `recover` as normal.

Note that this flag can make debugging crashes harder, as a panic will simply
exit the entire program without printing a stack trace, and source code
positions and many names are removed.
Similarly, `garble reverse` is generally not useful in this mode.

### Control flow obfuscation

See: [CONTROLFLOW.md](docs/CONTROLFLOW.md)

### Speed

`garble build` should take about twice as long as `go build`, as it needs to
complete two builds. The original build, to be able to load and type-check the
input code, and then the obfuscated build.

Garble obfuscates one package at a time, mirroring how Go compiles one package
at a time. This allows Garble to fully support Go's build cache; incremental
`garble build` calls should only re-build and re-obfuscate modified code.

Note that the first call to `garble build` may be comparatively slow,
as it has to obfuscate each package for the first time. This is akin to clearing
`GOCACHE` with `go clean -cache` and running a `go build` from scratch.

Garble also makes use of its own cache to reuse work, akin to Go's `GOCACHE`.
It defaults to a directory under your user's cache directory,
such as `~/.cache/garble`, and can be placed elsewhere by setting `GARBLE_CACHE`.

### Determinism and seeds

Just like Go, garble builds are deterministic and reproducible in nature.
This has significant benefits, such as caching builds and being able to use
`garble reverse` to de-obfuscate stack traces.

By default, garble will obfuscate each package in a unique way,
which will change if its build input changes: the version of garble, the version
of Go, the package's source code, or any build parameter such as GOOS or -tags.
This is a reasonable default since guessing those inputs is very hard.

You can use the `-seed` flag to provide your own obfuscation randomness seed.
Reusing the same seed can help produce the same code obfuscation,
which can help when debugging or reproducing problems.
Regularly rotating the seed can also help against reverse-engineering in the long run,
as otherwise one can look at changes in how Go's standard library is obfuscated
to guess when the Go or garble versions were changed across a series of builds.

To always use a different seed for each build, use `-seed=random`.
Note that extra care should be taken when using custom seeds:
if a `-seed` value used in a build is lost, `garble reverse` will not work.

### Caveats

Most of these can improve with time and effort. The purpose of this section is
to document the current shortcomings of this tool.

* Exported methods are never obfuscated at the moment, since they could
  be required by interfaces. This area is a work in progress; see
  [#3](https://github.com/burrowers/garble/issues/3).

* Aside from `GOGARBLE` to select patterns of packages to obfuscate,
  there is no supported way to exclude obfuscating a selection of files or packages.
  More often than not, a user would want to do this to work around a bug; please file the bug instead.

* Go programs [are initialized](https://go.dev/ref/spec#Program_initialization) one package at a time,
  where imported packages are always initialized before their importers,
  and otherwise they are initialized in the lexical order of their import paths.
  Since garble obfuscates import paths, this lexical order may change arbitrarily.

* Go plugins are not currently supported; see [#87](https://github.com/burrowers/garble/issues/87).

* Garble requires `git` to patch the linker. That can be avoided once go-gitdiff
  supports [non-strict patches](https://github.com/bluekeyes/go-gitdiff/issues/30).

* APIs like [`runtime.GOROOT`](https://pkg.go.dev/runtime#GOROOT)
  and [`runtime/debug.ReadBuildInfo`](https://pkg.go.dev/runtime/debug#ReadBuildInfo)
  will not work in obfuscated binaries. This [can affect loading timezones](https://github.com/golang/go/issues/51473#issuecomment-2490564684), for example.

### Contributing

We welcome new contributors. If you would like to contribute, see
[CONTRIBUTING.md](CONTRIBUTING.md) as a starting point.
