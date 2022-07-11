# garble

	go install mvdan.cc/garble@latest

Obfuscate Go code by wrapping the Go toolchain. Requires Go 1.18 or later.

	garble build [build flags] [packages]

The tool also supports `garble test` to run tests with obfuscated code,
and `garble reverse` to de-obfuscate text such as stack traces.
See `garble -h` for up to date usage information.

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

The tool obfuscates the packages matching `GOGARBLE`, a comma-separated list of
glob patterns of module path prefixes, as documented in `go help private`.
When `GOGARBLE` is empty, it assumes the value of `GOPRIVATE`.
When `GOPRIVATE` is also empty, then `GOGARBLE` assumes the value of the current
module path, to obfuscate all packages under the current module.

Note that commands like `garble build` will use the `go` version found in your
`$PATH`. To use different versions of Go, you can
[install them](https://go.dev/doc/manage-install#installing-multiple)
and set up `$PATH` with them. For example, for Go 1.17.1:

```sh
$ go install golang.org/dl/go1.17.1@latest
$ go1.17.1 download
$ PATH=$(go1.17.1 env GOROOT)/bin:${PATH} garble build
```

### Literal obfuscation

Using the `-literals` flag causes literal expressions such as strings to be
replaced with more complex expressions, resolving to the same value at run-time.
This feature is opt-in, as it can cause slow-downs depending on the input code.

Literals used in constant expressions cannot be obfuscated, since they are
resolved at compile time. This includes any expressions part of a `const`
declaration, for example.

### Tiny mode

With the `-tiny` flag, even more information is stripped from the Go binary.
Position information is removed entirely, rather than being obfuscated.
Runtime code which prints panics, fatal errors, and trace/debug info is removed.
All in all, this can make binaries 2-5% smaller.

With this flag, no panics or fatal runtime errors will ever be printed, but they
can still be handled internally with `recover` as normal. In addition, the
`GODEBUG` environmental variable will be ignored.

Note that this flag can make debugging crashes harder, as a panic will simply
exit the entire program without printing a stack trace, and all source code
positions are set to line 1. Similarly, `garble reverse` is generally not useful
in this mode.

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

### Determinism and seeds

Just like Go, garble builds are deterministic and reproducible in nature.
This has significant benefits, such as caching builds and being able to use
`garble reverse` to de-obfuscate stack traces.

By default, garble will obfuscate each package in a unique way,
which will change if its build input changes: the version of garble, the version
of Go, the package's source code, or any build parameter such as GOOS or -tags.
This is a reasonable default since guessing those inputs is very hard.

However, providing your own obfuscation seed via `-seed` brings some advantages.
For example, builds sharing the same seed will produce the same obfuscation,
even if any of the build parameters or versions vary.
It can also make reverse-engineering harder, as an end user could guess what
version of Go or garble you're using.

Note that extra care should be taken when using custom seeds.
If a seed used to build a binary gets lost, `garble reverse` will not work.
Rotating the seeds can also help against reverse-engineering in the long run,
as otherwise some bits of code may be obfuscated the same way over time.

An alternative approach is `-seed=random`, where each build is entirely different.

### Caveats

Most of these can improve with time and effort. The purpose of this section is
to document the current shortcomings of this tool.

* Exported methods are never obfuscated at the moment, since they could
  be required by interfaces. This area is a work in progress; see
  [#3](https://github.com/burrowers/garble/issues/3).

* Garble aims to automatically detect which Go types are used with reflection,
  as obfuscating those types might break your program.
  Note that Garble obfuscates [one package at a time](#speed),
  so if your reflection code inspects a type from an imported package,
  and your program broke, you may need to add a "hint" in the imported package:
   ```go
   type Message struct {
       Command string
       Args    string
   }

   // Never obfuscate the Message type.
   var _ = reflect.TypeOf(Message{})
   ```

* Go declarations exported for cgo via `//export` are not obfuscated.

* Go plugins are not currently supported; see [#87](https://github.com/burrowers/garble/issues/87).

### Contributing

We welcome new contributors. If you would like to contribute, see
[CONTRIBUTING.md](CONTRIBUTING.md) as a starting point.
