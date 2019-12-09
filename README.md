# garble

	GO111MODULE=on go get mvdan.cc/garble

Obfuscate a Go build. Requires Go 1.13 or later.

	garble build [build flags] [packages]

which is equivalent to the longer:

	go build -a -trimpath -toolexec=garble [build flags] [packages]

### Purpose

Produce a binary that works as well as a regular build, but that has as little
information about the original source code as possible.

The tool is designed to be:

* Coupled with `cmd/go`, to support both `GOPATH` and modules with ease
* Deterministic and reproducible, given the same initial source code
* Reversible given the original source, to un-garble panic stack traces

### Mechanism

The tool wraps calls to the Go compiler to transform the Go source code, in
order to:

* Replace as many useful identifiers as possible with short base64 hashes
* Remove [module build information](https://golang.org/pkg/runtime/debug/#ReadBuildInfo)
* Remove comments and empty lines, to make position info less useful

It also wraps calls to the linker in order to:

* Enforce the `-s` flag, to not include the symbol table
* Enforce the `-w` flag, to not include DWARF debugging data

Finally, the tool requires the use of the `-trimpath` build flag, to ensure the
binary doesn't include paths from the current filesystem.

### Caveats

The `-a` flag for `go build` is required, since `-toolexec` doesn't work well
with the build cache; see [#27628](https://github.com/golang/go/issues/27628).

Since no caching at all can take place right now (see the link above), builds
will be slower than `go build` - especially for large projects.

The standard library is never garbled when compiled, since the source is always
publicly available.
