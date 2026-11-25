package main

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
)

// Append the records before cmd/go hashes the archive for GOCACHE, so a
// link-only build uses the records belonging to the exact archives it opens.
func appendReflectArchiveMember(path string, names map[string]string) error {
	const member = "garble-reflect" // recognized by the patched linker
	var data strings.Builder
	for _, name := range slices.Sorted(maps.Keys(names)) {
		data.WriteString(name)
		data.WriteByte('\n')
		data.WriteString(names[name])
		data.WriteByte('\n')
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	// A Unix archive member header has fixed-width fields and even alignment.
	_, err = fmt.Fprintf(f, "%-16s%-12d%-6d%-6d%-8o%-10d`\n%s", member, 0, 0, 0, 0644, data.Len(), data.String())
	if err == nil && data.Len()%2 != 0 {
		_, err = f.Write([]byte{0})
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
