#!/bin/bash

# All packages that the runtime linknames to, except runtime and its dependencies.
# This resulting list is what we need to "go list" when obfuscating the runtime,
# as they are the packages that we may be missing.
comm -23 <(
	sed -rn 's@//go:linkname .* ([^.]*)\.[^.]*@\1@p' $(go env GOROOT)/src/runtime/*.go | grep -vE '^main|^runtime\.' | sort -u
) <(
	# Note that we assume this is constant across platforms.
	go list -deps runtime | sort -u
)
