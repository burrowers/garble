#!/bin/bash

# List of popular modules which we use as regression tests.
# Right now we only "garble build" them, but eventually it would be nice to use
# "garble test" with them as well, assuming they have good tests.
#
# The criteria to add modules to this list is that they should be:
#
#   * Popular; likely to be used in Go projects built with Garble
#   * Good quality; avoid projects with tons of generated code or slow tests
#   * Diverse; avoid multiple projects which are extremely similar
#
# For example, a good example of a project to add is one that has unearthed
# multiple bugs in garble before, such as Protobuf.
# Also remember that the standard library already provides significant cover.
modules=(
	# Protobuf helps cover encoding libraries and reflection.
	google.golang.org/protobuf v1.34.2

	# Wireguard helps cover networking and cryptography.
	# Note that the latest tag does not build on Go 1.23.
	golang.zx2c4.com/wireguard v0.0.0-20231211153847-12269c276173

	# Lo helps cover generics.
	# TODO: would be nice to find a more popular alternative,
	# at least once generics are more widespread.
	github.com/samber/lo v1.47.0

	# Brotli is a compression algorithm popular with HTTP.
	# It's also transpiled from C with a gitlab.com/cznic/ccgo,
	# so this helps stress garble with Go code that few humans would write.
	# Unlike other ccgo-generated projects, like modernc.org/sqlite,
	# it's relatively small without any transitive dependencies.
	github.com/andybalholm/brotli v1.1.0

	# TODO: consider github.com/mattn/go-sqlite3 to cover a DB and more cgo

	# TODO: add x/net and x/unix as they include hacks like go:linkname
	# or copying syscall types.
)

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)

exit_code=0

show() {
	echo "> ${@}"
	if ! "${@}"; then
		echo "FAIL"
		return 1
	fi
}

BASE_GOFLAGS="$(go env GOFLAGS)"
CACHED_MODFILES="${SCRIPT_DIR}/cached_modfiles"
mkdir -p "${CACHED_MODFILES}"

for ((i = 0; i < ${#modules[@]}; i += 2)); do
	module="${modules[i]}"
	version="${modules[i + 1]}"

	{
		# Initialize an empty module, so we can run "go build",
		# and add the module at the version that we want.
		# We cache the files between runs and commit them in git,
		# because each "go get module/...@version" is slow as it has to figure
		# out where the module lives, even if GOMODCACHE is populated.
		# Use the custom go.mod file for the rest of the commands via GOFLAGS.
		# We should delete these cached modfiles with major Go updates,
		# to recreate their "tidy" version with newer Go toolchains.
		cached_modfile="${module}"
		cached_modfile="${CACHED_MODFILES}/${cached_modfile//[^A-Za-z0-9._-]/_}.mod"
		export GOFLAGS="${BASE_GOFLAGS} -modfile=${cached_modfile}"

		if [[ ! -f "${cached_modfile}" ]]; then
			show go mod init test
			show go get "${module}/...@${version}"
		fi

		# Run "go build" first, to ensure the regular Go build works.
		# Use -trimpath to help reuse the cache with "garble build".
		show go build -trimpath "${module}/..."

		# Run the garble build.
		show garble build "${module}/..."

		# Also with more options.
		show garble -tiny -literals build "${module}/..."
	} || exit_code=1
done

exit ${exit_code}
