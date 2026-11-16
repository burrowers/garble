package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/rogpeppe/go-internal/testscript"
)

var (
	importsHashLine     = regexp.MustCompile(`^HASH\[[^]\r\n]+\](?:: (?:"(?:\\.|[^"\\])*"|[0-9a-f]{64}))?$`)
	importsSubkeyLine   = regexp.MustCompile(`^HASH subkey [0-9a-f]{64} "(?:\\.|[^"\\])*" = [0-9a-f]{64}$`)
	importsFileHashLine = regexp.MustCompile(`^HASH .+: [0-9a-f]{64}$`)
	importsBuildKey     = regexp.MustCompile(`^HASH\[build [^]\r\n]+\]: ([0-9a-f]{64})$`)
	importsIndex        = regexp.MustCompile(`^v1 ([0-9a-f]{64}) ([0-9a-f]{64}) ([ 0-9]{20}) ([ 0-9]{20})\n$`)
)

func splitImportsCacheHash(stderr string) (ordinary string, keys []string) {
	seen := make(map[string]bool)
	var out strings.Builder
	for len(stderr) > 0 {
		line, rest, found := strings.Cut(stderr, "\n")
		piece := line
		if found {
			piece += "\n"
		}
		stderr = rest
		clean := strings.TrimSuffix(line, "\r")
		if importsHashLine.MatchString(clean) || importsSubkeyLine.MatchString(clean) || importsFileHashLine.MatchString(clean) {
			if match := importsBuildKey.FindStringSubmatch(clean); match != nil && !seen[match[1]] {
				keys = append(keys, match[1])
				seen[match[1]] = true
			}
			continue
		}
		out.WriteString(piece)
	}
	return out.String(), keys
}

// A snapshot records filesystem observations after a build, not the Go command's earlier cache lookup or Put result.
func snapshotImportsCache(cacheDir string, keys []string) string {
	var out strings.Builder
	seen := make(map[string]bool)
	for _, key := range keys {
		if seen[key] {
			continue
		}
		seen[key] = true
		fmt.Fprintf(&out, "action=%s\n", key)
		if len(key) != 64 || !isLowerHex(key) {
			fmt.Fprintln(&out, "index_parse_error=invalid action key")
			continue
		}
		path := filepath.Join(cacheDir, key[:2], key+"-a")
		fmt.Fprintf(&out, "index_path=%s\n", path)
		f, err := os.Open(path)
		if err != nil {
			fmt.Fprintf(&out, "index_open_error=%v\n", err)
			continue
		}
		// The index is fixed-size; cap reads to avoid pulling unrelated data into memory.
		data, readErr := io.ReadAll(io.LimitReader(f, 256))
		closeErr := f.Close()
		if readErr != nil {
			fmt.Fprintf(&out, "index_read_error=%v\n", readErr)
			continue
		}
		if closeErr != nil {
			fmt.Fprintf(&out, "index_close_error=%v\n", closeErr)
		}
		fmt.Fprintf(&out, "index_content=%q\n", data)
		match := importsIndex.FindSubmatch(data)
		if match == nil || string(match[1]) != key {
			fmt.Fprintln(&out, "index_parse_error=invalid, truncated, or mismatched index")
			continue
		}
		size, sizeErr := strconv.ParseInt(strings.TrimSpace(string(match[3])), 10, 64)
		timestamp, timeErr := strconv.ParseInt(strings.TrimSpace(string(match[4])), 10, 64)
		if sizeErr != nil || timeErr != nil || size < 0 || timestamp < 0 {
			fmt.Fprintln(&out, "index_parse_error=invalid size or timestamp")
			continue
		}
		output := string(match[2])
		archive := filepath.Join(cacheDir, output[:2], output+"-d")
		fmt.Fprintf(&out, "output=%s index_size=%d index_timestamp_ns=%d archive_path=%s\n", output, size, timestamp, archive)
		a, err := os.Open(archive)
		if err != nil {
			fmt.Fprintf(&out, "archive_open_error=%v\n", err)
			continue
		}
		info, statErr := a.Stat()
		closeErr = a.Close()
		if statErr != nil {
			fmt.Fprintf(&out, "archive_stat_error=%v\n", statErr)
		} else {
			fmt.Fprintf(&out, "archive_size=%d size_match=%t\n", info.Size(), info.Size() == size)
		}
		if closeErr != nil {
			fmt.Fprintf(&out, "archive_close_error=%v\n", closeErr)
		}
	}
	return out.String()
}

func isLowerHex(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			if c < 'a' || c > 'f' {
				return false
			}
		}
	}
	return true
}

func cacheBuild(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) < 2 || (args[0] != "first" && args[0] != "second") {
		ts.Fatalf("usage: cache-build first|second garble-args...")
	}
	dir := ts.Getenv("IMPORTS_CACHE_DIAGNOSTICS")
	if dir == "" {
		root, err := filepath.Abs("cache_diagnostics")
		ts.Check(err)
		ts.Check(os.MkdirAll(root, 0o755))
		dir, err = os.MkdirTemp(root, "imports-")
		ts.Check(err)
		ts.Setenv("IMPORTS_CACHE_DIAGNOSTICS", dir)
	}
	oldDebug := ts.Getenv("GODEBUG")
	debug := "gocachehash=1"
	if oldDebug != "" {
		debug = oldDebug + "," + debug
	}
	ts.Setenv("GODEBUG", debug)
	err := ts.Exec("garble", args[1:]...)
	ts.Setenv("GODEBUG", oldDebug)
	stdout, stderr := ts.ReadFile("stdout"), ts.ReadFile("stderr")
	prefix := filepath.Join(dir, args[0])
	ts.Check(os.WriteFile(prefix+".stderr", []byte(stderr), 0o644))
	ts.Check(os.WriteFile(prefix+".stdout", []byte(stdout), 0o644))
	ordinary, keys := splitImportsCacheHash(stderr)
	ts.Check(os.WriteFile(prefix+".snapshot", []byte(snapshotImportsCache(ts.Getenv("GOCACHE"), keys)), 0o644))
	fmt.Fprint(ts.Stdout(), stdout)
	fmt.Fprint(ts.Stderr(), ordinary)
	if err != nil {
		ts.Fatalf("garble build failed: %v (diagnostics: %s)", err, dir)
	}
	if len(keys) == 0 {
		ts.Fatalf("no valid Go build action keys captured (diagnostics: %s)", dir)
	}
}
