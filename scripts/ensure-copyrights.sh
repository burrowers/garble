#!/bin/bash

for f in $(git ls-files '*.go'); do
	if ! grep -q Copyright $f; then
		sed -i '1i\
// Copyright (c) 2020, The Garble Authors.\
// See LICENSE for licensing information.\

' $f
	fi
done
