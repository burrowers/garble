// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/pkg/browser"
)

// bugIssueURL is the base URL for filing a new bug report against garble.
// The query parameters pre-fill fields in the bug issue form template; their
// names must match the field IDs in .github/ISSUE_TEMPLATE/00-bug.yml.
const bugIssueURL = "https://github.com/burrowers/garble/issues/new"

const bugIssueTemplate = "00-bug.yml"

// bugGoEnvWhitelist lists the `go env` variables included by default in a bug
// report. It covers what is diagnostically relevant for garble bugs while
// excluding variables that commonly contain user paths (GOPATH, GOMODCACHE,
// GOCACHE, GOBIN, GOENV, GOTMPDIR) to avoid leaking personal information.
// Users who need to share the full environment can pass -full-env.
var bugGoEnvWhitelist = []string{
	"GOOS",
	"GOARCH",
	"GOVERSION",
	"GOMOD",
	"GOWORK",
	"GOROOT",
	"GOFLAGS",
	"GOEXPERIMENT",
	"GOTOOLCHAIN",
	"CGO_ENABLED",
}

func commandBug(args []string) error {
	fs := flag.NewFlagSet("bug", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		flagPrint   bool
		flagFullEnv bool
	)
	fs.BoolVar(&flagPrint, "n", false, "Print the filled bug report template to stdout instead of opening a browser.")
	fs.BoolVar(&flagFullEnv, "full-env", false, "Include the full 'go env' output. By default, only a whitelist of\ndiagnostically relevant variables is included to avoid leaking user paths.")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `
Usage of 'garble bug':

	garble bug [-n] [-full-env]

Opens a web browser with a pre-filled bug report on the garble issue tracker.
The report includes the output of 'garble version' and 'go env'.

Flags:

`[1:])
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return errJustExit(0)
		}
		return errJustExit(2)
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return errJustExit(2)
	}

	garbleVersion := formatGarbleVersion()
	goEnv, err := collectGoEnv(flagFullEnv)
	if err != nil {
		return err
	}

	if flagPrint {
		fmt.Print(formatBugReport(garbleVersion, goEnv))
		return nil
	}

	target := buildBugIssueURL(garbleVersion, goEnv)
	fmt.Fprintf(os.Stderr, "Opening bug report in your browser...\n")
	if err := browser.OpenURL(target); err != nil {
		fmt.Fprintf(os.Stderr, "Could not open browser: %v\n", err)
		fmt.Fprintf(os.Stderr, "Please open the following URL manually:\n\n%s\n", target)
		return errJustExit(1)
	}
	return nil
}

// formatGarbleVersion returns the same content that the `garble version`
// subcommand prints to stdout, as a single string.
func formatGarbleVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	mod := &info.Main
	if mod.Replace != nil {
		mod = mod.Replace
	}
	var buf strings.Builder
	fmt.Fprintf(&buf, "%s %s\n\n", mod.Path, mod.Version)
	fmt.Fprintf(&buf, "Build settings:\n")
	for _, setting := range info.Settings {
		if setting.Value == "" {
			continue
		}
		fmt.Fprintf(&buf, "%16s %s\n", setting.Key, setting.Value)
	}
	return buf.String()
}

// collectGoEnv runs `go env` and returns its output. When full is false, only
// a whitelist of variables is returned. The output is formatted as KEY="VALUE"
// lines to match the format shown by `go env` without arguments, regardless of
// platform.
func collectGoEnv(full bool) (string, error) {
	args := []string{"env", "-json"}
	if !full {
		args = append(args, bugGoEnvWhitelist...)
	}
	out, err := exec.Command("go", args...).Output()
	if err != nil {
		return "", fmt.Errorf("running `go env` failed: %w", err)
	}
	var env map[string]string
	if err := json.Unmarshal(out, &env); err != nil {
		return "", fmt.Errorf("parsing `go env -json` output: %w", err)
	}

	keys := bugGoEnvWhitelist
	if full {
		keys = make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
	}

	var buf strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&buf, "%s=%q\n", k, env[k])
	}
	return buf.String(), nil
}

// formatBugReport returns a Markdown-rendered bug report suitable for manual
// copy-paste into an issue form.
func formatBugReport(garbleVersion, goEnv string) string {
	var buf strings.Builder
	buf.WriteString("### Output of `garble version`\n\n")
	buf.WriteString("```\n")
	buf.WriteString(strings.TrimRight(garbleVersion, "\n"))
	buf.WriteString("\n```\n\n")
	buf.WriteString("### Output of `go env` in your module/workspace\n\n")
	buf.WriteString("```shell\n")
	buf.WriteString(strings.TrimRight(goEnv, "\n"))
	buf.WriteString("\n```\n\n")
	buf.WriteString("### What did you do?\n\n")
	buf.WriteString("<!-- Provide clear steps for others to reproduce the error. -->\n\n")
	buf.WriteString("### What did you see happen?\n\n")
	buf.WriteString("<!-- Command invocations and their associated output. -->\n\n")
	buf.WriteString("### What did you expect to see?\n\n")
	buf.WriteString("<!-- Why is the current output incorrect, and any additional context. -->\n")
	return buf.String()
}

// buildBugIssueURL builds a URL that pre-fills the bug report issue form.
// Field names are keyed to .github/ISSUE_TEMPLATE/00-bug.yml.
func buildBugIssueURL(garbleVersion, goEnv string) string {
	q := url.Values{}
	q.Set("template", bugIssueTemplate)
	q.Set("garble-version", garbleVersion)
	q.Set("go-env", goEnv)
	return bugIssueURL + "?" + q.Encode()
}
