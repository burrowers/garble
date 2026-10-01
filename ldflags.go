package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

func computeLiteralLinkerFlags() error {
	sharedCache.LinkerFlags = nil
	linkerAliases = make(map[string][]string)
	if len(slices.Collect(flagValues(sharedCache.ForwardBuildFlags, "-ldflags"))) == 0 {
		return nil
	}
	sharedCache.LinkerFlags = make(map[string][]string)
	for path, pkg := range sharedCache.ListedPackages.all() {
		if pkg.Name != "main" || pkg.ForTest != "" {
			continue
		}
		if _, testing := sharedCache.ListedPackages.get(path + ".test"); testing {
			continue
		}
		flags, err := linkerFlags(pkg)
		if err != nil {
			return err
		}
		sharedCache.LinkerFlags[path] = flags
	}
	return nil
}

// linkerFlags mirrors cmd/go's last-matching per-package flag selection.
func linkerFlags(pkg *listedPackage) ([]string, error) {
	var flags []string
	for value := range flagValues(sharedCache.ForwardBuildFlags, "-ldflags") {
		value = strings.TrimSpace(value)
		if value != "" && !strings.HasPrefix(value, "-") {
			pattern, rest, ok := strings.Cut(value, "=")
			if !ok {
				return nil, fmt.Errorf("missing =<value> in <pattern>=<value>")
			}
			pattern = strings.TrimSpace(pattern)
			matches := linkerPatternMatches(pattern, pkg)
			if pattern == "work" || pattern == "tool" {
				var err error
				matches, err = linkerAliasMatches(pattern, pkg)
				if err != nil {
					return nil, err
				}
			}
			if !matches {
				continue
			}
			value = rest
		}
		var err error
		flags, err = cmdgoQuotedSplit(value)
		if err != nil {
			return nil, err
		}
	}
	return flags, nil
}

var linkerAliases = make(map[string][]string)

func linkerAliasMatches(pattern string, pkg *listedPackage) (bool, error) {
	paths, ok := linkerAliases[pattern]
	if !ok {
		args := []string{"list", "-e", "-f", "{{.ImportPath}}"}
		for _, name := range []string{"-mod", "-modfile", "-overlay", "-tags"} {
			for value := range flagValues(sharedCache.ForwardBuildFlags, name) {
				args = append(args, name+"="+value)
			}
		}
		args = append(args, pattern)
		out, err := exec.Command(sharedCache.GoCmd, args...).Output()
		if err != nil {
			return false, fmt.Errorf("resolve -ldflags package pattern %q: %w", pattern, err)
		}
		paths = strings.Fields(string(out))
		linkerAliases[pattern] = paths
	}
	for _, path := range paths {
		if pkg.ImportPath == path || pattern == "work" && pkg.ImportPath == path+".test" {
			return true, nil
		}
	}
	return false, nil
}

func linkerPatternMatches(pattern string, pkg *listedPackage) bool {
	switch pattern {
	case "all":
		return true
	case "std":
		return pkg.Standard
	case "cmd":
		return pkg.Standard && strings.HasPrefix(pkg.ImportPath, "cmd/")
	}
	if pattern == "." || pattern == ".." || strings.HasPrefix(pattern, "./") || strings.HasPrefix(pattern, "../") {
		// MatchPackage in cmd/go uses directory paths for relative patterns.
		dir := pattern
		suffix := ""
		if i := strings.Index(pattern, "..."); i >= 0 {
			j := strings.LastIndex(pattern[:i], "/")
			dir, suffix = pattern[:j], pattern[j+1:]
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			return false
		}
		if suffix == "" {
			return pkg.Dir == abs
		}
		rel, err := filepath.Rel(abs, pkg.Dir)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return false
		}
		return cmdgoPkgPatternMatchPattern(suffix)(filepath.ToSlash(rel))
	}
	return cmdgoPkgPatternMatchPattern(pattern)(pkg.ImportPath)
}
