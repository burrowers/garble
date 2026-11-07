// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"fmt"
	"go/token"
	"reflect"
	"strings"
	"testing"
)

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
