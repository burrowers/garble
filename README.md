# garble

	GO111MODULE=on go get mvdan.cc/garble

Obfuscate Go code by wrapping the Go toolchain. Requires Go 1.16 or later.

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
* Remove all [build](https://golang.org/pkg/runtime/#Version) and [module](https://golang.org/pkg/runtime/debug/#ReadBuildInfo) information
* Strip filenames and shuffle position information
* Strip debugging information and symbol tables via `-ldflags="-w -s"`
* [Obfuscate literals](#literal-obfuscation), if the `-literals` flag is given
* Remove [extra information](#tiny-mode), if the `-tiny` flag is given

By default, the tool obfuscates the packages under the current module. If not
running in module mode, then only the main package is obfuscated. To specify
what packages to obfuscate, set `GOPRIVATE`, documented at `go help private`.

Note that commands like `garble build` will use the `go` version found in your
`$PATH`. To use different versions of Go, you can
[install them](https://golang.org/doc/manage-install#installing-multiple)
and set up `$PATH` with them. For example, for Go 1.16.1:

```sh
$ go get golang.org/dl/go1.16.1
$ go1.16.1 download
$ PATH=$(go1.16.1 env GOROOT)/bin:${PATH} garble build
```

### Literal obfuscation

Using the `-literals` flag causes literal expressions such as strings to be
replaced with more complex variants, resolving to the same value at run-time.
This feature is opt-in, as it can cause slow-downs depending on the input code.

Literal expressions used as constants cannot be obfuscated, since they are
resolved at compile time. This includes any expressions part of a `const`
declaration.

### Tiny mode

When the `-tiny` flag is passed, extra information is stripped from the resulting
Go binary. This includes line numbers, filenames, and code in the runtime that
prints panics, fatal errors, and trace/debug info. All in all this can make binaries
2-5% smaller in our testing, as well as prevent extracting some more information.

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
input code, and finally the obfuscated build.

Go's build cache is fully supported; if a first `garble build` run is slow, a
second run should be significantly faster. This should offset the cost of the
double builds, as incremental builds in Go are fast.

### Determinism and seeds

Just like Go, garble builds are deterministic and reproducible if the inputs
remain the same: the version of Go, the version of Garble, and the input code.
This has significant benefits, such as caching builds or being able to use
`garble reverse` to de-obfuscate stack traces.

However, it also means that an input package will be obfuscated in exactly the
same way if none of those inputs change. If you want two builds of your program
to be entirely different, you can use `-seed` to provide a new seed for the
entire build, which will cause a full rebuild.

If any open source packages are being obfuscated, providing a custom seed can
also provide extra protection. It could be possible to guess the versions of Go
and garble given how a public package was obfuscated without a seed.

### Caveats

Most of these can improve with time and effort. The purpose of this section is
to document the current shortcomings of this tool.

* Exported methods are never obfuscated at the moment, since they could
  be required by interfaces and reflection. This area is a work in progress.

* It can be hard for garble to know what types will be used with
  [reflection](https://golang.org/pkg/reflect), including JSON encoding or
  decoding. If your program breaks because a type's names are obfuscated when
  they should not be, you can add an explicit hint:
	```go
	type Message struct {
		Command string
		Args    string
	}

	// Never obfuscate the Message type.
	var _ = reflect.TypeOf(Message{})
	```

* Go plugins are not currently supported; see [#87](https://github.com/burrowers/garble/issues/87).

### Contributing

We welcome new contributors. If you would like to contribute, see
[CONTRIBUTING.md](CONTRIBUTING.md) as a starting point.
