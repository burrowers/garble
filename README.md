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

* Coupled with `cmd/go`, to support both `GOPATH` and modules with ease
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
* Expose [additional functions](#runtime-api) in the runtime package that can
  optionally hide information during execution

### Options

By default, the tool garbles the packages under the current module. If not
running in module mode, then only the main package is garbled. To specify what
packages to garble, set `GOPRIVATE`, documented at `go help module-private`.

### Caveats

Most of these can improve with time and effort. The purpose of this section is
to document the current shortcomings of this tool.

* Build caching is not supported, so large projects will likely be slow to
  build. See [golang/go#41145](https://github.com/golang/go/issues/41145).

* Exported methods and fields are never garbled at the moment, since they could
  be required by interfaces and reflection. This area is a work in progress.

* Functions implemented outside Go, such as assembly, aren't garbled since we
  currently only transform the input Go source.

* Go plugins are not currently supported; see [#87](https://github.com/burrowers/garble/issues/87).

### Runtime API

The tool adds additional functions to the runtime that can optionally be used to
hide information during execution. The functions added are:

```go
// hideFatalErrors suppresses printing fatal error messages and
// fatal panics when hide is true. This behavior can be changed at 
// any time by calling hideFatalErrors again. All other behaviors of 
// panics remains the same.
func hideFatalErrors(hide bool)
```

These functions must be used with the `linkname` compiler directive, like so:

```go
package main

import _ "unsafe"

//go:linkname hideFatalErrors runtime.hideFatalErrors
func hideFatalErrors(hide bool)

func init() { hideFatalErrors(true) }

func main() {
	panic("ya like jazz?")
}
```
