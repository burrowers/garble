// Copyright (c) 2019, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"go/token"
	"io"
	"os"
	"os/exec"
	"strings"
)

const buildIDSeparator = "/"

// splitActionID returns the action ID half of a build ID, the first element.
func splitActionID(buildID string) string {
	i := strings.Index(buildID, buildIDSeparator)
	if i < 0 {
		return buildID
	}
	return buildID[:i]
}

// splitContentID returns the content ID half of a build ID, the last element.
func splitContentID(buildID string) string {
	return buildID[strings.LastIndex(buildID, buildIDSeparator)+1:]
}

// decodeHash is the opposite of hashToString, but with a panic for error
// handling since it should never happen.
func decodeHash(str string) []byte {
	h, err := base64.RawURLEncoding.DecodeString(str)
	if err != nil {
		panic(fmt.Sprintf("invalid hash %q: %v", str, err))
	}
	return h
}

func alterToolVersion(tool string, args []string) error {
	cmd := exec.Command(args[0], args[1:]...)
	out, err := cmd.Output()
	if err != nil {
		if err, _ := err.(*exec.ExitError); err != nil {
			return fmt.Errorf("%v: %s", err, err.Stderr)
		}
		return err
	}
	line := string(bytes.TrimSpace(out)) // no trailing newline
	f := strings.Fields(line)
	if len(f) < 3 || f[0] != tool || f[1] != "version" || f[2] == "devel" && !strings.HasPrefix(f[len(f)-1], "buildID=") {
		return fmt.Errorf("%s -V=full: unexpected output:\n\t%s", args[0], line)
	}
	var toolID []byte
	if f[2] == "devel" {
		// On the development branch, use the content ID part of the build ID.
		toolID = decodeHash(splitContentID(f[len(f)-1]))
	} else {
		// For a release, the output is like: "compile version go1.9.1 X:framepointer".
		// Use the whole line.
		toolID = []byte(line)
	}

	contentID, err := ownContentID(toolID)
	if err != nil {
		return fmt.Errorf("cannot obtain garble's own version: %v", err)
	}
	// The part of the build ID that matters is the last, since it's the
	// "content ID" which is used to work out whether there is a need to redo
	// the action (build) or not. Since cmd/go parses the last word in the
	// output as "buildID=...", we simply add "+garble buildID=_/_/_/${hash}".
	// The slashes let us imitate a full binary build ID, but we assume that
	// the other components such as the action ID are not necessary, since the
	// only reader here is cmd/go and it only consumes the content ID.
	fmt.Printf("%s +garble buildID=_/_/_/%s\n", line, contentID)
	return nil
}

func ownContentID(toolID []byte) (string, error) {
	// We can't rely on the module version to exist, because it's
	// missing in local builds without 'go get'.
	// For now, use 'go tool buildid' on the binary that's running. Just
	// like Go's own cache, we use hex-encoded sha256 sums.
	// Once https://github.com/golang/go/issues/37475 is fixed, we
	// can likely just use that.
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	buildID, err := buildidOf(path)
	if err != nil {
		return "", err
	}
	ownID := decodeHash(splitContentID(buildID))

	// Join the two content IDs together into a single base64-encoded sha256
	// sum. This includes the original tool's content ID, and garble's own
	// content ID.
	h := sha256.New()
	h.Write(toolID)
	h.Write(ownID)

	// We also need to add the selected options to the full version string,
	// because all of them result in different output. We use spaces to
	// separate the env vars and flags, to reduce the chances of collisions.
	if envGoPrivate != "" {
		fmt.Fprintf(h, " GOPRIVATE=%s", envGoPrivate)
	}
	if opts.GarbleLiterals {
		fmt.Fprintf(h, " -literals")
	}
	if opts.Tiny {
		fmt.Fprintf(h, " -tiny")
	}
	if len(opts.Seed) > 0 {
		fmt.Fprintf(h, " -seed=%x", opts.Seed)
	}

	return hashToString(h.Sum(nil)), nil
}

// hashToString encodes the first 120 bits of a sha256 sum in base64, the same
// format used for elements in a build ID.
func hashToString(h []byte) string {
	return base64.RawURLEncoding.EncodeToString(h[:15])
}

func buildidOf(path string) (string, error) {
	cmd := exec.Command("go", "tool", "buildid", path)
	out, err := cmd.Output()
	if err != nil {
		if err, _ := err.(*exec.ExitError); err != nil {
			return "", fmt.Errorf("%v: %s", err, err.Stderr)
		}
		return "", err
	}
	return string(out), nil
}

var (
	// Hashed names are base64-encoded.
	// Go names can only be letters, numbers, and underscores.
	// This means we can use base64's URL encoding, minus '-'.
	// Use the URL encoding, replacing '-' with a duplicate 'z'.
	// Such a lossy encoding is fine, since we never decode hashes.
	nameCharset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_z"
	nameBase64  = base64.NewEncoding(nameCharset)
)

func hashWith(salt []byte, name string) string {
	if len(salt) == 0 {
		panic("hashWith: empty salt")
	}
	if name == "" {
		panic("hashWith: empty name")
	}
	const length = 4

	d := sha256.New()
	d.Write(salt)
	d.Write(opts.Seed)
	io.WriteString(d, name)
	sum := nameBase64.EncodeToString(d.Sum(nil))

	// TODO: Just make the first letter uppercase or lowercase as needed.
	// This is also not needed for non-names, like import paths.
	if token.IsExported(name) {
		return "Z" + sum[:length]
	}
	return "z" + sum[:length]
}
