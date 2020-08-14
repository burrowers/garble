# garble

	GO111MODULE=on go get mvdan.cc/garble

Obfuscate a Go build. Requires Go 1.14 or later.

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
* Remove all [build](https://golang.org/pkg/runtime/#Version) and [module](https://golang.org/pkg/runtime/debug/#ReadBuildInfo) information
* Strip filenames and shuffle position information
* Obfuscate literals, if the `-literals` flag is given
* Strip debugging information and symbol tables
* Expose additional functions in the runtime that can optionally hide
  information during execution

### Options

By default, the tool garbles the packages under the current module. If not
running in module mode, then only the main package is garbled. To specify what
packages to garble, set `GOPRIVATE`, documented at `go help module-private`.

### Caveats

Most of these can improve with time and effort. The purpose of this section is
to document the current shortcomings of this tool.

* Package import path names are never garbled, since we require the original
  paths for the build system to work. See #13 to investigate alternatives.

* The `-a` flag for `go build` is required, since `-toolexec` doesn't work well
  with the build cache; see [golang/go#27628](https://github.com/golang/go/issues/27628).

* Since no caching at all can take place right now (see the link above), fast
  incremental builds aren't possible. Large projects might be slow to build.

* Deciding what method names to garble is always going to be difficult, due to
  interfaces that could be implemented up or down the package import tree. At
  the moment, exported methods are never garbled.

* Similarly to methods, exported struct fields are difficult to garble, as the
  names might be relevant for reflection work like `encoding/json`. At the
  moment, exported methods are never garbled.

* Functions implemented outside Go, such as assembly, aren't garbled since we
  currently only transform the input Go source.

* Since `garble` forces `-trimpath`, plugins built with `-garble` must be loaded
  from Go programs built with `-trimpath` too.

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
