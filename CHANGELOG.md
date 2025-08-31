# Changelog

## [v0.15.0] - 2025-08-31

This release adds support for Go 1.25 and drops support for Go 1.23
and Go 1.24.

Literal obfuscation is improved slightly so that deobfuscation via
static analysis is not as easy to achieve.

Attempting to obfuscate packages which inject function headers
into the runtime via `//go:linkname` now fails in a very clear way,
as such packages like `github.com/bytedance/sonic/loader` cannot work
with an obfuscated runtime.

A number of fixes are also included:
* Fix obfuscating packages whose Go files all import `C`
* Fix builds where `GOROOT` is a symbolic link
* Fix control flow obfuscation on packages importing `unsafe`
* Fix a regression where build flags were not obeyed in `garble reverse`

## [v0.14.2] - 2025-04-13

This bugfix release fixes a number of issues and continues support
for Go 1.23 and 1.24.

Toolchain upgrades via `GOTOOLCHAIN` now work correctly; the transparent
upgrade could lead to "linked object header mismatch" errors as garble
was accidentally mixing the original and upgraded toolchain versions.

`garble -debugdir` now refuses to delete a non-empty directory if its
contents were not created by a previous `-debugdir` invocation.
This should prevent mistakes which could lead to losing important files.

Function intrinsics were not being picked up correctly from Go 1.24;
this could lead to degraded performance for some users, as obfuscating
their names prevented the toolchain from optimizing them.

## [v0.14.1] - 2025-02-12

This release adds support for Go 1.24 and continues support for Go 1.23.

## [v0.14.0] - 2025-01-26

This release drops support for Go 1.22 and continues support for Go 1.23.

@lu4p improved the compatibility with reflection of Go types by collecting
the set of all types used with reflection during the entire build,
and then inject the de-obfuscation of their names in the link step.
Thanks to this, many more Go packages should work out of the box,
and the README caveat suggesting the use of "reflection hints" is removed.

@mvdan replaced our own tracking of type aliases, necessary given that the
alias name becomes a field name when embedded into a struct type.
We now rely entirely on upstream Go's tracking of aliases in `go/types`.
Note that this means that Garble now requires Go 1.23.5 or later,
given that alias tracking did not work properly in previous Go versions.

A number of fixes are also included:
* Reduce the amount of info fetched from `go list -json` for a ~2% speed-up 
* Package names and paths are now obfuscated separately
* Hashing of struct types to obfuscate field names is now better implemented
* Fix a panic which could occur when using structs as type parameters

## [v0.13.0] - 2024-09-05

This release drops support for Go 1.21 and adds support for Go 1.23.

A number of fixes are also included:
* Fix obfuscation errors when arch-dependent struct padding is used
* Fix a failure when using garble inside a `go.work` workspace
* Fail early and clearly if the Go version is too new
* Rewrite the main `go generate` script from Bash to Go and improve it

## [v0.12.1] - 2024-02-18

This bugfix release fixes a regression in v0.12.0 that broke `x/sys/unix`.
See #830.

## [v0.12.0] - 2024-02-10

This release continues support for Go 1.21 and includes fixes for Go 1.22,
now that the final 1.22.0 release is out.

@lu4p improved the detection of types used with reflection to track `make` calls too,
fixing more `cannot use T1 as T2` errors when obfuscating types. See [#690].

@pagran added a trash block generator to the control flow obfuscator.
See [#825].

A number of bugfixes are also included:
* Avoid an error when building for `GOOS=ios` - [#816]
* Prevent the shuffle literal obfuscation from being optimized away - [#819]
* Support inline comments in assembly `#include` lines - [#812]

## [v0.11.0] - 2023-12-02

This release drops support for Go 1.20, continues support for Go 1.21,
and adds initial support for the upcoming Go 1.22.

@lu4p and @mvdan improved the code using SSA to detect which types are used with reflection,
which should fix a number of errors such as `cannot use T1 as T2` or `cannot convert T1 to T2`.
See: [#685], [#763], [#782], [#785], [#807].

@pagran added experimental support for control flow obfuscation,
which should provide stronger obfuscation of function bodies when enabled.
See the documentation at [docs/CONTROLFLOW.md](https://github.com/burrowers/garble/blob/master/docs/CONTROLFLOW.md).
See [#462].

A number of bugfixes are also included:

* Avoid panicking on a struct embedding a builtin alias - [#798]
* Strip struct field tags when hashing struct types for type identity - [#801]

## [v0.10.1] - 2023-06-25

This bugfix release continues support for Go 1.20 and the upcoming 1.21,
and features:

* Avoid obfuscating local types used for reflection, like in `go-spew` - #765

## [v0.10.0] - 2023-06-05

This release drops support for Go 1.19, continues support for Go 1.20,
and adds initial support for the upcoming Go 1.21.

@lu4p rewrote the code to detect whether `reflect` is used on each Go type,
which is used to decide which Go types should not be obfuscated to prevent breakage.
The old code analyzed syntax trees with type information, which is cheap but clumsy.
The new code uses SSA, which adds a bit of CPU cost to builds, but allows for a
more powerful analysis that is less likely to break on edge cases.
While this change does slow down builds slightly, we will start using SSA for more
features in the near term, such as control flow obfuscation. See [#732].

@pagran improved the patching of Go's linker to also obfuscate funcInfo.entryoff,
making it harder to relate a function's metadata with its body in the binary. See [#641].

@mvdan rewrote garble's caching to be more robust, avoiding errors such as
"cannot load garble export file". The new caching system is entirely separate
from Go's `GOCACHE`, being placed in `GARBLE_CACHE`, which defaults to a directory
such as `~/.cache/garble`. See [#708].

@DominicBreuker taught `-literals` to support obfuscating large string literals
by using the "simple" obfuscator on them, as it runs in linear time. See [#720].

@mvdan added support for `garble run`, the obfuscated version of `go run`,
to quickly test that a main program still works when obfuscated. See [#661].

A number of bugfixes are also included:

* Ensure that `sync/atomic` types are still aligned by the compiler - [#686]
* Print the chosen random seed when using `-seed=random` - [#696]
* Avoid errors in `git apply` if the system language isn't English - [#698]
* Avoid a panic when importing a missing package - [#694]
* Suggest a command when asking the user to rebuild garble - [#739]

## [v0.9.3] - 2023-02-12

This bugfix release continues support for Go 1.19 and 1.20, and features:

* Support inline comments in assembly to fix `GOARCH=ppc64` - [#672]
* Avoid obfuscating `reflect.Value` to fix `davecgh/go-spew` - [#676]
* Fix runtime panics when using `garble build` inside a VCS directory - [#675]

## [v0.9.2] - 2023-02-07

This bugfix release continues support for Go 1.19 and 1.20, and features:

* Support `go:linkname` directives referencing methods - [#656]
* Fix more "unused import" errors with `-literals` - [#658]

## [v0.9.1] - 2023-01-26

This bugfix release continues support for Go 1.19 and the upcoming Go 1.20,
and features:

* Support obfuscating code which uses "dot imports" - [#610]
* Fix linking errors for MIPS architectures - [#646]
* Compiler intrinsics for packages like `math/bits` work again - [#655]

## [v0.9.0] - 2023-01-17

This release continues support for Go 1.19 and the upcoming Go 1.20.

Noteworthy changes include:

* Randomize the magic number header in `pclntab` - [#622]
* Further reduce binary sizes with `-tiny` by 4%  - [#633]
* Reduce the size overhead of all builds by 2% - [#629]
* Reduce the binary size overhead of `-literals` by 20%  - [#637]
* Support assembly references to the current package name - [#619]
* Support package paths with periods in assembly - [#621]

Note that the first two changes are done by patching and rebuilding Go's linker.
While this adds complexity, it enables more link time obfuscation.

## [v0.8.0] - 2022-12-15

This release drops support for Go 1.18, continues support for Go 1.19,
and adds initial support for the upcoming Go 1.20.

Noteworthy changes include:

* `GOGARBLE=*` is now the default to obfuscate all packages - [#594]
* `GOPRIVATE` is no longer used, being deprecated in [v0.5.0]
* Obfuscate assembly source code filenames - [#605]
* Randomize the lengths of obfuscated names
* Support obfuscating `time` and `syscall`
* Avoid reflect method call panics if `reflect` is obfuscated

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

[v0.14.2]: https://github.com/burrowers/garble/releases/tag/v0.14.2
[v0.14.1]: https://github.com/burrowers/garble/releases/tag/v0.14.1
[v0.14.0]: https://github.com/burrowers/garble/releases/tag/v0.14.0
[v0.13.0]: https://github.com/burrowers/garble/releases/tag/v0.13.0

[v0.12.0]: https://github.com/burrowers/garble/releases/tag/v0.12.0
[#690]: https://github.com/burrowers/garble/issues/690
[#812]: https://github.com/burrowers/garble/issues/812
[#816]: https://github.com/burrowers/garble/pull/816
[#819]: https://github.com/burrowers/garble/pull/819
[#825]: https://github.com/burrowers/garble/pull/825

[v0.11.0]: https://github.com/burrowers/garble/releases/tag/v0.11.0
[#462]: https://github.com/burrowers/garble/issues/462
[#685]: https://github.com/burrowers/garble/issues/685
[#763]: https://github.com/burrowers/garble/issues/763
[#782]: https://github.com/burrowers/garble/issues/782
[#785]: https://github.com/burrowers/garble/issues/785
[#798]: https://github.com/burrowers/garble/issues/798
[#801]: https://github.com/burrowers/garble/issues/801
[#807]: https://github.com/burrowers/garble/issues/807

[v0.10.1]: https://github.com/burrowers/garble/releases/tag/v0.10.1

[v0.10.0]: https://github.com/burrowers/garble/releases/tag/v0.10.0
[#641]: https://github.com/burrowers/garble/pull/641
[#661]: https://github.com/burrowers/garble/issues/661
[#686]: https://github.com/burrowers/garble/issues/686
[#694]: https://github.com/burrowers/garble/issues/694
[#696]: https://github.com/burrowers/garble/issues/696
[#698]: https://github.com/burrowers/garble/issues/698
[#708]: https://github.com/burrowers/garble/issues/708
[#720]: https://github.com/burrowers/garble/pull/720
[#732]: https://github.com/burrowers/garble/pull/732
[#739]: https://github.com/burrowers/garble/pull/739

[v0.9.3]: https://github.com/burrowers/garble/releases/tag/v0.9.3
[#672]: https://github.com/burrowers/garble/issues/672
[#675]: https://github.com/burrowers/garble/pull/675
[#676]: https://github.com/burrowers/garble/issues/676

[v0.9.2]: https://github.com/burrowers/garble/releases/tag/v0.9.2
[#656]: https://github.com/burrowers/garble/issues/656
[#658]: https://github.com/burrowers/garble/issues/658

[v0.9.1]: https://github.com/burrowers/garble/releases/tag/v0.9.1
[#610]: https://github.com/burrowers/garble/issues/610
[#646]: https://github.com/burrowers/garble/issues/646
[#655]: https://github.com/burrowers/garble/pull/655

[v0.9.0]: https://github.com/burrowers/garble/releases/tag/v0.9.0
[#619]: https://github.com/burrowers/garble/issues/619
[#621]: https://github.com/burrowers/garble/issues/621
[#622]: https://github.com/burrowers/garble/issues/622
[#629]: https://github.com/burrowers/garble/pull/629
[#633]: https://github.com/burrowers/garble/pull/633
[#637]: https://github.com/burrowers/garble/pull/637

[v0.8.0]: https://github.com/burrowers/garble/releases/tag/v0.8.0
[#594]: https://github.com/burrowers/garble/issues/594
[#605]: https://github.com/burrowers/garble/issues/605

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
