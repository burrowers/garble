package main

import (
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLinkerVariablePatterns(t *testing.T) {
	old := sharedCache
	defer func() { sharedCache = old }()
	sharedCache = &sharedCacheType{ListedPackages: newListedPackages()}
	sharedCache.ListedPackages.set("example.com/cmd", &listedPackage{Name: "main", ImportPath: "example.com/cmd", Dir: "/project/cmd", Imports: []string{"example.com/lib"}})
	sharedCache.ListedPackages.set("example.com/lib", &listedPackage{Name: "lib", ImportPath: "example.com/lib"})
	pkg := types.NewPackage("example.com/lib", "lib")
	obj := types.NewVar(0, pkg, "Version", types.Typ[types.String])
	pkg.Scope().Insert(obj)
	for _, tc := range []struct {
		flags []string
		want  string
	}{
		{[]string{"-ldflags=all=-X example.com/lib.Version=all_value"}, "all_value"},
		{[]string{"-ldflags=example.com/...=-X example.com/lib.Version=pattern_value"}, "pattern_value"},
		{[]string{"-ldflags=other/...=-X example.com/lib.Version=wrong_value"}, ""},
		{[]string{"-ldflags=-X example.com/lib.Version=default_value", "-ldflags=other/...=-X example.com/lib.Version=wrong_value"}, "default_value"},
	} {
		sharedCache.ForwardBuildFlags = tc.flags
		if err := computeLiteralLinkerFlags(); err != nil {
			t.Fatal(err)
		}
		got, err := computeLinkerVariableStrings(pkg, false)
		if err != nil {
			t.Fatal(err)
		}
		if got[obj] != tc.want {
			t.Errorf("%q: got %q, want %q", tc.flags, got[obj], tc.want)
		}
	}
	sharedCache.ListedPackages.set("example.com/other", &listedPackage{Name: "main", ImportPath: "example.com/other", Imports: []string{"example.com/lib"}})
	sharedCache.ForwardBuildFlags = []string{"-ldflags=example.com/cmd=-X example.com/lib.Version=only_one_main"}
	if err := computeLiteralLinkerFlags(); err != nil {
		t.Fatal(err)
	}
	if _, err := computeLinkerVariableStrings(pkg, false); err == nil {
		t.Fatal("accepted conflicting injection across commands sharing a dependency")
	}
}

func TestLinkerPatternBuildHash(t *testing.T) {
	oldCache, oldLiterals := sharedCache, flagLiterals
	defer func() { sharedCache, flagLiterals = oldCache, oldLiterals }()
	root := t.TempDir()
	for _, dir := range []string{"cmd", "other"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(root)
	flagLiterals = true
	sharedCache = &sharedCacheType{BinaryContentID: []byte("garble-test"), ForwardBuildFlags: []string{"-ldflags=./cmd=-X main.Version=pattern_value"}, ListedPackages: newListedPackages()}
	sharedCache.ListedPackages.set("example.com/cmd", &listedPackage{Name: "main", ImportPath: "example.com/cmd", Dir: filepath.Join(root, "cmd")})
	if err := computeLiteralLinkerFlags(); err != nil {
		t.Fatal(err)
	}
	before := addGarbleToHash([]byte("same-action"))
	t.Chdir(filepath.Join(root, "other"))
	if err := computeLiteralLinkerFlags(); err != nil {
		t.Fatal(err)
	}
	after := addGarbleToHash([]byte("same-action"))
	if before == after {
		t.Fatal("relative linker pattern reused a compilation hash after its meaning changed")
	}
}

func TestLinkerFlagsCacheRoundTrip(t *testing.T) {
	original := &sharedCacheType{ListedPackages: newListedPackages(), LinkerFlags: map[string][]string{"example.com/cmd": {"-X", "example.com/lib.Version= foo bar ", "-X=main.Empty="}, "example.com/other": nil}}
	data, err := original.MarshalMsg(nil)
	if err != nil {
		t.Fatal(err)
	}
	var decoded sharedCacheType
	rest, err := decoded.UnmarshalMsg(data)
	if err != nil || len(rest) != 0 {
		t.Fatalf("decode: %v, trailing bytes: %d", err, len(rest))
	}
	if !reflect.DeepEqual(original.LinkerFlags, decoded.LinkerFlags) {
		t.Fatalf("linker flags changed: %#v", decoded.LinkerFlags)
	}
}

func TestLinkerFlagAliases(t *testing.T) {
	oldCache, oldAliases := sharedCache, linkerAliases
	defer func() { sharedCache, linkerAliases = oldCache, oldAliases }()
	goCmd, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	sharedCache = &sharedCacheType{GoCmd: goCmd}
	for _, tc := range []struct {
		pattern, path string
		want          bool
	}{
		{"work", "mvdan.cc/garble", true},
		{"work", "mvdan.cc/garble.test", true},
		{"work", "golang.org/x/tools/cmd/bundle", false},
		{"tool", "golang.org/x/tools/cmd/bundle", true},
		{"tool", "mvdan.cc/garble", false},
	} {
		got, err := linkerAliasMatches(tc.pattern, &listedPackage{ImportPath: tc.path})
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("%s(%s) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}
