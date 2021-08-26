# Changelog

## [0.4.0] - 2021-08-26

This release of Garble adds support for Go 1.17.x while maintaining support for
Go 1.16.x. A few other noteworthy changes are included:

* Support obfuscating literals in more edge cases with `-literals`
* Improve detection of `reflect` usage with standard library APIs
* Names exported for cgo are no longer obfuscated
* Avoid breaking consts using `iota` with `-literals`

Known bugs:

* obfuscating the entire standard library with `GOPRIVATE=*` is not well supported yet

## [0.3.0] - 2021-05-31

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

## [0.2.0] - 2021-04-08

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

## [0.1.0] - 2021-03-05

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

[0.4.0]: https://github.com/burrowers/garble/releases/tag/v0.4.0
[0.3.0]: https://github.com/burrowers/garble/releases/tag/v0.3.0
[0.2.0]: https://github.com/burrowers/garble/releases/tag/v0.2.0
[0.1.0]: https://github.com/burrowers/garble/releases/tag/v0.1.0
