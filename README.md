# garble

	GO111MODULE=on go get mvdan.cc/garble

Obfuscate Go code by wrapping the Go toolchain. Requires Go 1.16 or later.

	garble build [build flags] [packages]

See `garble -h` for up to date usage information.

### Purpose

Produce a binary that works as well as a regular build, but that has as little
information about the original source code as possible.

The tool is designed to be:

* Coupled with `cmd/go`, to support modules and build caching
* Deterministic and reproducible, given the same initial source code
* Reversible given the original source, to deobfuscate panic stack traces

### Mechanism

The tool wraps calls to the Go compiler and linker to transform the Go build, in
order to:

* Replace as many useful identifiers as possible with short base64 hashes
* Replace package paths with short base64 hashes
* Remove all [build](https://golang.org/pkg/runtime/#Version) and [module](https://golang.org/pkg/runtime/debug/#ReadBuildInfo) information
* Strip filenames and shuffle position information
* Strip debugging information and symbol tables
* Obfuscate literals, if the `-literals` flag is given
* Remove [extra information](#tiny-mode) if the `-tiny` flag is given

### Options

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

You can also declare a function to make multiple uses simpler:

```sh
$ withgo() {
	local gocmd=go${1}
	shift

	PATH=$(${gocmd} env GOROOT)/bin:${PATH} "$@"
}
$ withgo 1.16.1 garble build
```

### Caveats

Most of these can improve with time and effort. The purpose of this section is
to document the current shortcomings of this tool.

* Exported methods are never obfuscated at the moment, since they could
  be required by interfaces and reflection. This area is a work in progress.

* Go plugins are not currently supported; see [#87](https://github.com/burrowers/garble/issues/87).

* There are cases where garble is a little too agressive with obfuscation, this may lead to identifiers getting obfuscated which are needed for reflection, e.g. to parse JSON into a struct; see [#162](https://github.com/burrowers/garble/issues/162). To work around this you can pass a hint to garble, that an type is used for reflection via passing it to `reflect.TypeOf` or `reflect.ValueOf` in the same file:
	```go
	// this is used for parsing json
	type Message struct {
		Command string
		Args    string
	}

	// never obfuscate the Message type
	var _ = reflect.TypeOf(Message{})
	```

### Tiny Mode

When the `-tiny` flag is passed, extra information is stripped from the resulting
Go binary. This includes line numbers, filenames, and code in the runtime that
prints panics, fatal errors, and trace/debug info. All in all this can make binaries
2-5% smaller in our testing.

Note: if `-tiny` is passed, no panics, fatal errors will ever be printed, but they can
still be handled internally with `recover` as normal. In addition, the `GODEBUG`
environmental variable will be ignored.

### Contributing

We actively seek new contributors, if you would like to contribute to garble use the
[CONTRIBUTING.md](CONTRIBUTING.md) as a starting point.
