#!/bin/bash

if \
	grep \
		--recursive \
		--files-with-matches \
		--binary \
		--binary-files=without-match \
		--max-count=1 \
		--exclude-dir="\.git" \
		$'\r' \
		. \
	; then
	# TODO exit status should be number of files with wrong endings found
	echo -e "Found at least a file with CRLF endings."
	exit 1
fi

echo -e "No files with CRLF endings found."
exit 0
