// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"crypto/sha256"
	"fmt"
	"go/token"
	"reflect"
	"strings"
	"testing"
)

func TestWordListNameOutput(t *testing.T) {
	old := sharedCache
	t.Cleanup(func() { sharedCache = old })
	for _, test := range []struct {
		name  string
		words string
		sum   []byte
		input string
		want  string
	}{
		{"Binary", "a\nb", []byte{0x05}, "private", "b_a_b" + strings.Repeat("_a", 45)},
		{"Ternary", "a\nb\nc", []byte{11}, "private", "c_a_b" + strings.Repeat("_a", 28)},
		{"Exported", "a\nb\nc", []byte{11}, "Public", "C_a_b" + strings.Repeat("_a", 28)},
		{"PackagePath", "a\nb\nc", []byte{11}, "example.org/pkg", "c_a_b" + strings.Repeat("_a", 28)},
		{"Longest", "aaaaaaa\nbbbbbbb\nccccccc\nddddddd\neeeeeee\nfffffff\nggggggg\nhhhhhhh", []byte{0}, "Public", "Aaaaaaa" + strings.Repeat("_aaaaaaa", 15)},
	} {
		t.Run(test.name, func(t *testing.T) {
			words, count, err := parseWordList(test.words)
			if err != nil {
				t.Fatal(err)
			}
			sharedCache = &sharedCacheType{Words: words, WordCount: count}
			if got := wordListName(test.sum, test.input); got != test.want {
				t.Fatalf("wordListName = %q, want %q", got, test.want)
			}
		})
	}
}

func BenchmarkWordListName(b *testing.B) {
	old := sharedCache
	b.Cleanup(func() { sharedCache = old })
	sum := sha256.Sum256([]byte("word-list naming benchmark"))
	for _, size := range []int{2, 8, 256, 65536} {
		var text strings.Builder
		for i := range size {
			// Small dictionaries need short words to satisfy the name limit.
			width := 1
			if size > 26 {
				width = 4
			}
			value := i
			for range width {
				text.WriteByte('a' + byte(value%26))
				value /= 26
			}
			text.WriteByte('\n')
		}
		words, count, err := parseWordList(text.String())
		if err != nil {
			b.Fatal(err)
		}
		sharedCache = &sharedCacheType{Words: words, WordCount: count}
		for _, name := range []string{"private", "Exported"} {
			b.Run(fmt.Sprintf("%d/%s", size, name), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					wordListName(sum[:], name)
				}
			})
		}
	}
}

func TestWordListValidation(t *testing.T) {
	for _, text := range []string{"", "one\none", "A\nb", "a1\nb", "a_b\nb", "é\nb", "abcdefghi\nb", "apple\npear"} {
		if _, _, err := parseWordList(text); err == nil {
			t.Errorf("accepted %q", text)
		}
	}
}

func TestWordListNames(t *testing.T) {
	words, count, err := parseWordList("b\na\nb\n")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(words, ",") != "a,b" || count != 48 {
		t.Fatalf("%v %d", words, count)
	}
	old := sharedCache
	defer func() { sharedCache = old }()
	sharedCache = &sharedCacheType{Words: words, WordCount: count}
	for _, name := range []string{"Exported", "private", "example.org/pkg"} {
		got := hashWithCustomSalt([]byte("salt"), name)
		if !token.IsIdentifier(got) || strings.Count(got, "_") != count-1 {
			t.Fatalf("invalid word name %q", got)
		}
		if token.IsIdentifier(name) && token.IsExported(got) != token.IsExported(name) {
			t.Fatal(got)
		}
		if got != hashWithCustomSalt([]byte("salt"), name) {
			t.Fatal("not deterministic")
		}
		if got == hashWithCustomSalt([]byte("other"), name) {
			t.Fatal("salt ignored")
		}
	}
	seen := make(map[string]bool)
	for i := 0; i < 10000; i++ {
		got := hashWithCustomSalt([]byte("salt"), fmt.Sprintf("name%d", i))
		if seen[got] {
			t.Fatalf("collision: %s", got)
		}
		seen[got] = true
	}
}

func TestWordListCache(t *testing.T) {
	old := sharedCache
	defer func() { sharedCache = old }()
	words, count, err := parseWordList(" a\r\nb\na\n")
	if err != nil {
		t.Fatal(err)
	}
	sharedCache = &sharedCacheType{Words: words, WordCount: count, BinaryContentID: []byte("binary"), ListedPackages: newListedPackages()}
	hash := addGarbleToHash([]byte("input"))
	data, err := sharedCache.MarshalMsg(nil)
	if err != nil {
		t.Fatal(err)
	}
	decoded := new(sharedCacheType)
	if _, err := decoded.UnmarshalMsg(data); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.Words, words) || decoded.WordCount != count {
		t.Fatalf("lost snapshot: %#v", decoded)
	}
	sharedCache = decoded
	if addGarbleToHash([]byte("input")) != hash {
		t.Fatal("snapshot changed build key")
	}
	sharedCache.Words = []string{"a", "c"}
	if addGarbleToHash([]byte("input")) == hash {
		t.Fatal("list content ignored")
	}
	sharedCache.Words = nil
	if got := hashWithCustomSalt([]byte("salt"), "private"); len(got) < minHashLength || len(got) > maxHashLength {
		t.Fatal("default naming changed")
	}
}
