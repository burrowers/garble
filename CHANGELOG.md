# Changelog

## Unreleased

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

[0.2.0]: https://github.com/burrowers/garble/releases/tag/v0.2.0
[0.1.0]: https://github.com/burrowers/garble/releases/tag/v0.1.0
