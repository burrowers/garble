#!/bin/bash

# The list of all packages the runtime source linknames to.
linked="$(sed -rn 's@//go:linkname .* ([^.]*)\.[^.]*@\1@p' $(go env GOROOT)/src/runtime/*.go | grep -vE '^main|^runtime\.' | sort -u)"

# The list of all implied dependencies of the packages above,
# across all main GOOS targets.
implied="$(for GOOS in linux darwin windows js; do
	for pkg in $linked; do
		GOOS=$GOOS GOARCH=$GOARCH go list -e -deps $pkg | grep -v '^'$pkg'$'
	done
done | sort -u)"

# All packages in linked, except those implied by others already.
# This resulting list is what we need to "go list" when obfuscating the runtime,
# as they are the packages that we may be missing.
comm -23 <(
	echo "$linked"
) <(
	echo "$implied"
)
