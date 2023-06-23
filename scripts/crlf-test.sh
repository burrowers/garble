#!/bin/bash

file_count=$(grep \
	--recursive \
	--files-with-matches \
	--binary \
	--binary-files=without-match \
	--max-count=1 \
	--exclude-dir="\.git" \
	$'\r' \
	. | wc -l)

if [[ $file_count -gt 0 ]]; then
	echo -e "Found $file_count file(s) with CRLF endings."
	exit "$file_count"
fi

echo -e "No files with CRLF endings found."
exit 0
