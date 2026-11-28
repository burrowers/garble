// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// The patched Go driver preserves Garble's flags and "toolexec" command,
// replacing only the tool and its arguments with a private JSON response file.
func readToolArgs(args []string) ([]string, error) {
	if len(args) == 1 && strings.HasPrefix(args[0], "@") {
		data, err := os.ReadFile(args[0][1:])
		if err != nil {
			return nil, fmt.Errorf("cannot read tool arguments: %w", err)
		}
		if err := json.Unmarshal(data, &args); err != nil {
			return nil, fmt.Errorf("cannot decode tool arguments: %w", err)
		}
	}
	if len(args) == 0 || args[0] == "" {
		return nil, fmt.Errorf("missing toolexec tool")
	}
	return args, nil
}

// Source rewriting can grow an argument list which was small enough for Go
// to pass directly. Only objabi.Flagparse tools understand these response files.
func toolResponseFile(cmd *exec.Cmd, tool string) (func(), error) {
	noop := func() {}
	switch tool {
	case "compile", "asm", "link", "cgo", "cover", "pack":
	default:
		return noop, nil
	}
	// Allow for Windows quoting and separators, not just the raw byte count.
	size := 0
	for _, arg := range cmd.Args {
		size += 2*len(arg) + 3
	}
	if size <= 30<<10 {
		return noop, nil
	}
	file, err := os.CreateTemp(sharedTempDir, "tool-args-*")
	if err != nil {
		return nil, err
	}
	cleanup := func() { os.Remove(file.Name()) }
	var buf strings.Builder
	escape := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "`", "\\`")
	for _, arg := range cmd.Args[1:] {
		buf.WriteByte('"')
		buf.WriteString(escape.Replace(arg))
		buf.WriteString("\"\n")
	}
	_, writeErr := file.WriteString(buf.String())
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		cleanup()
		if writeErr != nil {
			return nil, writeErr
		}
		return nil, closeErr
	}
	cmd.Args = []string{cmd.Args[0], "@" + file.Name()}
	return cleanup, nil
}
