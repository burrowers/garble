# garble

	GO111MODULE=on go get mvdan.cc/garble

Obfuscate Go code by wrapping the Go toolchain. Requires Go 1.15 or later, since
Go 1.14 uses an entirely different object format.

	garble build [build flags] [packages]

See `garble -h` for up to date usage information.

### Purpose

Produce a binary that works as well as a regular build, but that has as little
information about the original source code as possible.

The tool is designed to be:

* Coupled with `cmd/go`, to support modules and build caching
* Deterministic and reproducible, given the same initial source code
* Reversible given the original source, to un-garble panic stack traces

### Mechanism

The tool wraps calls to the Go compiler and linker to transform the Go build, in
order to:

* Replace as many useful identifiers as possible with short base64 hashes
* Replace package paths with short base64 hashes
* Remove all [build](https://golang.org/pkg/runtime/#Version) and [module](https://golang.org/pkg/runtime/debug/#ReadBuildInfo) information
* Strip filenames and shuffle position information
* Strip debugging information and symbol tables
* Obfuscate literals, if the `-literals` flag is given
* Removes [extra information](#tiny-mode) if the `-tiny` flag is given

### Options

By default, the tool garbles the packages under the current module. If not
running in module mode, then only the main package is garbled. To specify what
packages to garble, set `GOPRIVATE`, documented at `go help module-private`.

### Caveats

Most of these can improve with time and effort. The purpose of this section is
to document the current shortcomings of this tool.

* Exported methods and fields are never garbled at the moment, since they could
  be required by interfaces and reflection. This area is a work in progress.

* Functions implemented outside Go, such as assembly, aren't garbled since we
  currently only transform the input Go source.

* Go plugins are not currently supported; see [#87](https://github.com/burrowers/garble/issues/87).

### Tiny Mode

When the `-tiny` flag is passed, extra information is stripped from the resulting 
Go binary. This includes line numbers, filenames, and code in the runtime the 
prints panics, fatal errors, and trace/debug info. All in all this can make binaries 
6-10% smaller in our testing.

Note: if `-tiny` is passed, no panics, fatal errors will ever be printed, but they can
still be handled internally with `recover` as normal. In addition, the `GODEBUG` 
environmental variable will be ignored.
