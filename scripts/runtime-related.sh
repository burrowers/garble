#!/bin/bash

# This script is hacky, but lets us list all packages depended on by runtime, or
# related to runtime via go:linkname.
#
# Once we can obfuscate the runtime package, this script can probably be
# deleted.

go version
echo

for GOOS in linux darwin windows; do
	skip="|macos"
	if [[ $GOOS == "darwin" ]]; then
		skip=""
	fi

	GOOS=$GOOS go list -deps $(sed -rn 's@//go:linkname .* ([^.]*)\.[^.]*@\1@p' $(go env GOROOT)/src/runtime/*.go | grep -vE '^main|^runtime\.|js'$skip) runtime || exit 1
done | sort -u
