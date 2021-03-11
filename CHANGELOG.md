# Changelog

## Unreleased

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

[0.1.0]: https://github.com/burrowers/garble/releases/tag/v0.1.0
