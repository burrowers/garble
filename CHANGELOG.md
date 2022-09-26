# Changelog

## [v0.7.2] - 2022-09-26

This bugfix release continues support for Go 1.18 and 1.19 and features:

* Fix an edge case resulting in bad syntax due to comments - [#573]
* Avoid a panic involving generic code - [#577]
* Obfuscate Go names in assembly header files - [#553]
* Support `garble reverse` on packages using cgo or assembly - [#555]

## [v0.7.1] - 2022-08-02

This bugfix release finishes support for Go 1.19 and features:

* Obfuscate all cgo filenames to not leak import paths
* Support obfuscating `net` and `runtime/debug`
* Don't leak temporary directories after obfuscating
* Fix an edge case resulting in broken import declarations
* Reduce allocations involved in obfuscating code

## [v0.7.0] - 2022-06-10

This release drops support for Go 1.17, continues support for Go 1.18,
and adds initial support for the upcoming Go 1.19.

Noteworthy changes include:

* Initial support for obfuscating generic code - [#414]
* Remove unused imports in `-literals` more reliably - [#481]
* Support obfuscating package paths ending with `.go` - [#539]
* Support installing garble in paths containing spaces - [#544]
* Avoid a panic when obfuscating variadic functions - [#524]
* Avoid a "refusing to list package" panic in `garble test` - [#522]
* Some module builds are now used as regression tests - [#240]

## [v0.6.0] - 2022-03-22

This release adds support for Go 1.18 while continuing support for Go 1.17.x.
Note that building generic code isn't supported just yet.

Noteworthy changes include:

* Obfuscation is now fully deterministic with a fixed `-seed` - [#449]
* Improve support for type aliases to fix some build failures - [#466]
* Add support for quotes in `-ldflags` as per `go help build` - [#492]
* Fail if the current Go version is newer than what built garble - [#269]
* Various optimizations resulting in builds being up to 5% faster - [#456]

## [v0.5.1] - 2022-01-18

This bugfix release features:

* Obfuscate exported names in `main` packages
* Fix build errors when using `-literals` with `GOGARBLE=*`
* Avoid breaking `-ldflags=-X` when `-literals` is used
* Avoid link errors when using `-debugdir`
* Speed up obfuscating the `runtime` package

## [v0.5.0] - 2022-01-06

This release of Garble adds initial support for the upcoming Go 1.18,
continues support for Go 1.17.x, and drops support for Go 1.16.x.
Note that building generic code isn't supported just yet.

Two breaking changes are introduced:

* Deprecate the use of `GOPRIVATE` in favor of `GOGARBLE` (see https://github.com/burrowers/garble/issues/276)
* `garble reverse` now requires a main package argument

Noteworthy changes include:

* Improve detection of `reflect` usage even further
* Support obfuscating some more standard library packages
* Improve literal obfuscation by using constant folding
* Add the `-debug` flag to log details of the obfuscated build
* Ensure the `runtime` package is built in a reproducible way
* Obfuscate local variable names to prevent shadowing bugs
* Fix and test support for using garble on 32-bit hosts

## [v0.4.0] - 2021-08-26

This release of Garble adds support for Go 1.17.x while maintaining support for
Go 1.16.x. A few other noteworthy changes are included:

* Support obfuscating literals in more edge cases with `-literals`
* Improve detection of `reflect` usage with standard library APIs
* Names exported for cgo are no longer obfuscated
* Avoid breaking consts using `iota` with `-literals`

Known bugs:

* obfuscating the entire standard library with `GOPRIVATE=*` is not well supported yet

## [v0.3.0] - 2021-05-31

This release of Garble fixes a number of bugs and improves existing features,
while maintaining support for Go 1.16.x. Notably:

* Make builds reproducible even when cleaning `GOCACHE`
* Detecting types used with reflection is more reliable
* Cross builds with `GOPRIVATE=*` are now supported
* Support conversion between struct types from different packages
* Improve support for type aliases
* Function names used with `go:linkname` are now obfuscated
* `garble reverse` can now reverse field names and lone filenames

Known bugs:

* obfuscating the entire standard library with `GOPRIVATE=*` is not well supported yet

## [v0.2.0] - 2021-04-08

This release of Garble drops support for Go 1.15.x, which is necessary for some
of the enhancements below:

* New: `garble test` allows running Go tests built with obfuscation
* New: `garble reverse` allows de-obfuscating output like stack traces
* Names of functions implemented in assembly are now obfuscated
* `GOPRIVATE=*` now works with packages like `crypto/tls` and `embed`
* `garble build` can now be used with many main packages at once
* `-literals` is more robust and now works on all of `std`

The README is also overhauled to be more helpful to first-time users.

Known bugs:

* obfuscating the entire standard library with `GOPRIVATE=*` is not well supported yet

## [v0.1.0] - 2021-03-05

This is the first release of Garble. It supports Go 1.15.x and 1.16.x.

It ships all the major features that have worked for the past year, including:

* Obfuscation of all names, except methods and reflect targets
* Obfuscation of package import paths and position information
* Stripping of build and module information
* Support for Go modules
* Reproducible and cacheable builds
* Stripping of extra information via `-tiny`
* Literal obfuscation via `-literals`

Known bugs:

* obfuscating the standard library with `GOPRIVATE=*` is not well supported yet
* `garble test` is temporarily disabled, as it is currently broken

[v0.7.2]: https://github.com/burrowers/garble/releases/tag/v0.7.2
[#573]: https://github.com/burrowers/garble/issues/573
[#577]: https://github.com/burrowers/garble/issues/577
[#553]: https://github.com/burrowers/garble/issues/553
[#555]: https://github.com/burrowers/garble/issues/555

[v0.7.1]: https://github.com/burrowers/garble/releases/tag/v0.7.1

[v0.7.0]: https://github.com/burrowers/garble/releases/tag/v0.7.0
[#240]: https://github.com/burrowers/garble/issues/240
[#414]: https://github.com/burrowers/garble/issues/414
[#481]: https://github.com/burrowers/garble/issues/481
[#522]: https://github.com/burrowers/garble/issues/522
[#524]: https://github.com/burrowers/garble/issues/524
[#539]: https://github.com/burrowers/garble/issues/539
[#544]: https://github.com/burrowers/garble/issues/544

[v0.6.0]: https://github.com/burrowers/garble/releases/tag/v0.6.0
[#449]: https://github.com/burrowers/garble/issues/449
[#466]: https://github.com/burrowers/garble/issues/466
[#492]: https://github.com/burrowers/garble/issues/492
[#269]: https://github.com/burrowers/garble/issues/269
[#456]: https://github.com/burrowers/garble/issues/456

[v0.5.1]: https://github.com/burrowers/garble/releases/tag/v0.5.1
[v0.5.0]: https://github.com/burrowers/garble/releases/tag/v0.5.0
[v0.4.0]: https://github.com/burrowers/garble/releases/tag/v0.4.0
[v0.3.0]: https://github.com/burrowers/garble/releases/tag/v0.3.0
[v0.2.0]: https://github.com/burrowers/garble/releases/tag/v0.2.0
[v0.1.0]: https://github.com/burrowers/garble/releases/tag/v0.1.0
