#!/bin/bash
echo "searching for CRLF endings in: ."

BOLD_RED='\033[1;31m'
BOLD_GREEN='\033[1;32m'
NC='\033[0m'

ERROR_COUNT=0

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
  echo -e "${BOLD_RED}Found at least a file with CRLF endings.${NC}"
  exit 1
fi

echo -e "${BOLD_GREEN}No files with CRLF endings found.${NC}"
exit 0