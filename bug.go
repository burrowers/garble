// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/pkg/browser"
)

// bugIssueURL is the base URL for filing a new bug report against garble.
// The query parameters pre-fill fields in the bug issue form template; their
// names must match the field IDs in .github/ISSUE_TEMPLATE/00-bug.yml.
const bugIssueURL = "https://github.com/burrowers/garble/issues/new"

const bugIssueTemplate = "00-bug.yml"

func commandBug(args []string) error {
	fs := flag.NewFlagSet("bug", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `
Usage of 'garble bug':

	garble bug

Opens a web browser with a pre-filled bug report on the garble issue tracker.
The report includes the output of 'garble version', 'go version', and
'go env -changed'.
`[1:])
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

	var garbleVersion strings.Builder
	writeGarbleVersion(&garbleVersion)

	goVersion, err := exec.Command("go", "version").Output()
	if err != nil {
		return fmt.Errorf("running `go version` failed: %w", err)
	}

	goEnv, err := collectGoEnv()
	if err != nil {
		return err
	}

	q := url.Values{}
	q.Set("template", bugIssueTemplate)
	q.Set("garble-version", garbleVersion.String())
	q.Set("go-version", string(bytes.TrimSpace(goVersion)))
	q.Set("go-env", goEnv)
	target := bugIssueURL + "?" + q.Encode()

	fmt.Fprintf(os.Stderr, "Opening bug report in your browser at:\n%s\n", target)
	// Honor $BROWSER before falling back to pkg/browser's platform defaults.
	// This mirrors what `go bug` does and keeps the command scriptable.
	if cmd := os.Getenv("BROWSER"); cmd != "" {
		c := exec.Command(cmd, target)
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		return c.Run()
	}
	if err := browser.OpenURL(target); err != nil {
		fmt.Fprintf(os.Stderr, "Could not open browser: %v\n", err)
		return errJustExit(1)
	}
	return nil
}

// collectGoEnv runs `go env -changed -json` and formats its output as
// KEY="VALUE" lines, consistent across platforms. Only variables whose values
// differ from the toolchain defaults are included, which keeps user-specific
// paths out of the report unless the user has set them explicitly.
func collectGoEnv() (string, error) {
	out, err := exec.Command("go", "env", "-changed", "-json").Output()
	if err != nil {
		return "", fmt.Errorf("running `go env -changed` failed: %w", err)
	}
	var env map[string]string
	if err := json.Unmarshal(out, &env); err != nil {
		return "", fmt.Errorf("parsing `go env -changed -json` output: %w", err)
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var buf strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&buf, "%s=%q\n", k, env[k])
	}
	return buf.String(), nil
}
