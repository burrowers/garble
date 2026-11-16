package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSplitImportsCacheHash(t *testing.T) {
	key := strings.Repeat("a", 64)
	input := "ordinary before\r\nHASH[build database/sql]\r\nHASH[build database/sql]: \"quoted \\\"input\\\"\"\nHASH subkey " + key + " \"desc\" = " + key + "\nHASH /tmp/example.go: " + key + "\nHASH[build database/sql]: " + key + "\nHASH[build test/main]: " + key + "\nHASH[build database/sql]: " + key + "\nHASH[build database/sql]: not a hash\nHASH[build database/sql]: \"unfinished\nordinary trailing"
	ordinary, keys := splitImportsCacheHash(input)
	want := "ordinary before\r\nHASH[build database/sql]: not a hash\nHASH[build database/sql]: \"unfinished\nordinary trailing"
	if ordinary != want {
		t.Fatalf("ordinary = %q, want %q", ordinary, want)
	}
	if fmt.Sprint(keys) != fmt.Sprint([]string{key}) {
		t.Fatalf("keys = %v", keys)
	}
}

func TestSplitImportsCacheHashLongLine(t *testing.T) {
	long := strings.Repeat("x", 200000)
	ordinary, keys := splitImportsCacheHash(long + "\nHASH[build test/main]: " + strings.Repeat("b", 64) + "\n")
	if ordinary != long+"\n" || len(keys) != 1 {
		t.Fatalf("long ordinary line or build key lost: length=%d keys=%v", len(ordinary), keys)
	}
}

func TestSnapshotImportsCache(t *testing.T) {
	root := t.TempDir()
	key := strings.Repeat("a", 64)
	output := strings.Repeat("b", 64)
	index := filepath.Join(root, key[:2], key+"-a")
	archive := filepath.Join(root, output[:2], output+"-d")
	if err := os.MkdirAll(filepath.Dir(index), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(archive), 0o755); err != nil {
		t.Fatal(err)
	}
	entry := func(id, out string, size int64) string {
		return fmt.Sprintf("v1 %s %s %20d %20d\n", id, out, size, 123)
	}
	check := func(want string) {
		t.Helper()
		got := snapshotImportsCache(root, []string{key, key})
		if strings.Count(got, "action="+key) != 1 || !strings.Contains(got, want) {
			t.Fatalf("snapshot = %q; want %q once", got, want)
		}
	}
	check("index_open_error=")
	if runtime.GOOS != "windows" {
		if err := os.Mkdir(index, 0o755); err != nil {
			t.Fatal(err)
		}
		check("index_read_error=")
		if err := os.Remove(index); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ name, content, want string }{
		{"truncated", "v1 " + key, "index_parse_error="},
		{"oversized", entry(key, output, 4) + "extra", "index_parse_error="},
		{"bad header", "v2" + entry(key, output, 4)[2:], "index_parse_error="},
		{"mismatched", entry(strings.Repeat("c", 64), output, 4), "index_parse_error="},
		{"invalid output", entry(key, strings.Repeat("z", 64), 4), "index_parse_error="},
		{"negative size", entry(key, output, -1), "index_parse_error="},
		{"invalid timestamp", strings.Replace(entry(key, output, 4), fmt.Sprintf("%20d", 123), fmt.Sprintf("%20s", "bad"), 1), "index_parse_error="},
		{"valid absent archive", entry(key, output, 4), "archive_open_error="},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(index, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			check(tc.want)
		})
	}
	if err := os.WriteFile(archive, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(index, []byte(entry(key, output, 4)), 0o644); err != nil {
		t.Fatal(err)
	}
	check("size_match=false")
	if err := os.WriteFile(archive, []byte("abcd"), 0o644); err != nil {
		t.Fatal(err)
	}
	check("size_match=true")
}
