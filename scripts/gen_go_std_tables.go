// Copyright (c) 2024, The Garble Authors.
// See LICENSE for licensing information.

//go:build ignore

// This is a program used with `go generate`, so it handles errors via panic.
package main

import (
	"bytes"
	"cmp"
	"fmt"
	"go/format"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/template"
)

var tmplTables = template.Must(template.New("").Parse(`
// Code generated by scripts/gen_go_std_tables.go; DO NOT EDIT.

// Generated from Go version {{ .GoVersion }}.

package main

var runtimeAndDeps = map[string]bool{
{{- range $path := .RuntimeAndDeps }}
	"{{ $path }}": true,
{{- end }}
}

var runtimeLinknamed = []string{
{{- range $path := .RuntimeLinknamed }}
	"{{ $path }}",
{{- end }}
	// The net package linknames to the runtime, not the other way around.
	// TODO: support this automatically via our script.
	"net",
}

var compilerIntrinsics = map[string]map[string]bool{
{{- range $intr := .CompilerIntrinsics }}
	"{{ $intr.Path }}": {
{{- range $name := $intr.Names }}
		"{{ $name }}": true,
{{- end }}
	},
{{- end }}
}

var reflectSkipPkg = map[string]bool{
	"fmt": true,
}
`[1:]))

type tmplData struct {
	GoVersion          string
	RuntimeAndDeps     []string
	RuntimeLinknamed   []string
	CompilerIntrinsics []tmplIntrinsic
}

type tmplIntrinsic struct {
	Path  string
	Names []string
}

func (t tmplIntrinsic) Compare(t2 tmplIntrinsic) int {
	return cmp.Compare(t.Path, t2.Path)
}

func (t tmplIntrinsic) Equal(t2 tmplIntrinsic) bool {
	return t.Compare(t2) == 0
}

func cmdGo(args ...string) string {
	cmd := exec.Command("go", args...)
	out, err := cmd.Output()
	if err != nil {
		panic(err)
	}
	return string(bytes.TrimSpace(out)) // no trailing newline
}

func readFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func sortedLines(s string) []string {
	lines := strings.Split(s, "\n")
	slices.Sort(lines)
	lines = slices.Compact(lines)
	return lines
}

var rxLinkname = regexp.MustCompile(`^//go:linkname .* ([^.]*)\.[^.]*$`)
var rxIntrinsic = regexp.MustCompile(`\b(addF|alias)\("([^"]*)", "([^"]*)",`)

func main() {
	goversion := cmdGo("env", "GOVERSION") // not "go version", to exclude GOOS/GOARCH
	goroot := cmdGo("env", "GOROOT")

	runtimeAndDeps := sortedLines(cmdGo("list", "-deps", "runtime"))

	// All packages that the runtime linknames to, except runtime and its dependencies.
	// This resulting list is what we need to "go list" when obfuscating the runtime,
	// as they are the packages that we may be missing.
	var runtimeLinknamed []string
	runtimeGoFiles, err := filepath.Glob(filepath.Join(goroot, "src", "runtime", "*.go"))
	if err != nil {
		panic(err)
	}
	for _, goFile := range runtimeGoFiles {
		for _, line := range strings.Split(readFile(goFile), "\n") {
			m := rxLinkname.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			path := m[1]
			switch path {
			case "main", "runtime/metrics_test":
				continue
			}
			runtimeLinknamed = append(runtimeLinknamed, path)
		}
	}
	slices.Sort(runtimeLinknamed)
	runtimeLinknamed = slices.Compact(runtimeLinknamed)
	runtimeLinknamed = slices.DeleteFunc(runtimeLinknamed, func(path string) bool {
		return slices.Contains(runtimeAndDeps, path)
	})

	compilerIntrinsicsIndexByPath := make(map[string]int)
	var compilerIntrinsics []tmplIntrinsic
	for _, line := range strings.Split(readFile(filepath.Join(
		goroot, "src", "cmd", "compile", "internal", "ssagen", "ssa.go",
	)), "\n") {
		m := rxIntrinsic.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		path, name := m[2], m[3]
		if i := compilerIntrinsicsIndexByPath[path]; i == 0 {
			compilerIntrinsicsIndexByPath[path] = len(compilerIntrinsics)
			compilerIntrinsics = append(compilerIntrinsics, tmplIntrinsic{
				Path:  path,
				Names: []string{name},
			})
		} else {
			compilerIntrinsics[i].Names = append(compilerIntrinsics[i].Names, name)
		}
	}
	slices.SortFunc(compilerIntrinsics, tmplIntrinsic.Compare)
	compilerIntrinsics = slices.CompactFunc(compilerIntrinsics, tmplIntrinsic.Equal)
	for path := range compilerIntrinsics {
		intr := &compilerIntrinsics[path]
		slices.Sort(intr.Names)
		intr.Names = slices.Compact(intr.Names)
	}

	var buf bytes.Buffer
	if err := tmplTables.Execute(&buf, tmplData{
		GoVersion:          goversion,
		RuntimeAndDeps:     runtimeAndDeps,
		RuntimeLinknamed:   runtimeLinknamed,
		CompilerIntrinsics: compilerIntrinsics,
	}); err != nil {
		panic(err)
	}
	out := buf.Bytes()
	formatted, err := format.Source(out)
	if err != nil {
		fmt.Println(string(out))
		panic(err)
	}

	if err := os.WriteFile("go_std_tables.go", formatted, 0o666); err != nil {
		panic(err)
	}
}
