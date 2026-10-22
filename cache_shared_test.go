// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"reflect"
	"testing"
)

func TestListedTestMainCacheRoundTrip(t *testing.T) {
	original := listedPackage{
		Name:       "main",
		ImportPath: "example/p.test",
		BuildID:    "test-main-build",
		GoFiles:    []string{"/cache/generated-test-main-d"},
		Imports:    []string{"testing", "example/p"},
		ImportMap:  map[string]string{"example/p": "example/p [example/p.test]"},
	}
	data, err := original.MarshalMsg(nil)
	if err != nil {
		t.Fatal(err)
	}
	var decoded listedPackage
	rest, err := decoded.UnmarshalMsg(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 0 || !reflect.DeepEqual(original, decoded) {
		t.Fatalf("test main changed during cache round trip: %#v", decoded)
	}
	if decoded.toObfuscate() {
		t.Fatal("retaining generated test-main source must not enable obfuscation")
	}
}
