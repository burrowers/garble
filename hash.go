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
	"os/exec"
	"strings"
)

const buildIDSeparator = "/"

// splitActionID returns the action ID half of a build ID, the first component.
func splitActionID(buildID string) string {
	return buildID[:strings.Index(buildID, buildIDSeparator)]
}

// splitContentID returns the content ID half of a build ID, the last component.
func splitContentID(buildID string) string {
	return buildID[strings.LastIndex(buildID, buildIDSeparator)+1:]
}

// decodeHash is the opposite of hashToString, with a panic for error handling
// since it should never happen.
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
		// Use the whole line, as we can assume it's unique.
		toolID = []byte(line)
	}

	contentID := addGarbleToHash(toolID)
	// The part of the build ID that matters is the last, since it's the
	// "content ID" which is used to work out whether there is a need to redo
	// the action (build) or not. Since cmd/go parses the last word in the
	// output as "buildID=...", we simply add "+garble buildID=_/_/_/${hash}".
	// The slashes let us imitate a full binary build ID, but we assume that
	// the other components such as the action ID are not necessary, since the
	// only reader here is cmd/go and it only consumes the content ID.
	fmt.Printf("%s +garble buildID=_/_/_/%s\n", line, hashToString(contentID))
	return nil
}

// addGarbleToHash takes some arbitrary input bytes,
// typically a hash such as an action ID or a content ID,
// and returns a new hash which also contains garble's own deterministic inputs.
//
// This includes garble's own version, obtained via its own binary's content ID,
// as well as any other options which affect a build, such as GOPRIVATE and -tiny.
func addGarbleToHash(inputHash []byte) []byte {
	// Join the two content IDs together into a single base64-encoded sha256
	// sum. This includes the original tool's content ID, and garble's own
	// content ID.
	h := sha256.New()
	h.Write(inputHash)
	if len(cache.BinaryContentID) == 0 {
		panic("missing binary content ID")
	}
	h.Write(cache.BinaryContentID)

	// We also need to add the selected options to the full version string,
	// because all of them result in different output. We use spaces to
	// separate the env vars and flags, to reduce the chances of collisions.
	if cache.GoEnv.GOPRIVATE != "" {
		fmt.Fprintf(h, " GOPRIVATE=%s", cache.GoEnv.GOPRIVATE)
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

	return h.Sum(nil)[:buildIDComponentLength]
}

// buildIDComponentLength is the number of bytes each build ID component takes,
// such as an action ID or a content ID.
const buildIDComponentLength = 15

// hashToString encodes the first 120 bits of a sha256 sum in base64, the same
// format used for components in a build ID.
func hashToString(h []byte) string {
	return base64.RawURLEncoding.EncodeToString(h[:buildIDComponentLength])
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

// These funcs mimic the unicode package API, but byte-based since we know
// base64 is all ASCII.

func isDigit(b byte) bool { return '0' <= b && b <= '9' }
func isLower(b byte) bool { return 'a' <= b && b <= 'z' }
func isUpper(b byte) bool { return 'A' <= b && b <= 'Z' }
func toLower(b byte) byte { return b + ('a' - 'A') }
func toUpper(b byte) byte { return b - ('a' - 'A') }

// hashWith returns a hashed version of name, including the provided salt as well as
// opts.Seed into the hash input.
//
// The result is always four bytes long. If the input was a valid identifier,
// the output remains equally exported or unexported. Note that this process is
// reproducible, but not reversible.
func hashWith(salt []byte, name string) string {
	if len(salt) == 0 {
		panic("hashWith: empty salt")
	}
	if name == "" {
		panic("hashWith: empty name")
	}
	// hashLength is the number of base64 characters to use for the final
	// hashed name.
	// This needs to be long enough to realistically avoid hash collisions,
	// but short enough to not bloat binary sizes.
	// The namespace for collisions is generally a single package, since
	// that's where most hashed names are namespaced to.
	// Using a "hash collision" formula, and taking a generous estimate of a
	// package having 10k names, we get the following probabilities.
	// Most packages will have far fewer names, but some packages are huge,
	// especially generated ones.
	// We also have slightly fewer bits in practice, since the base64
	// charset has 'z' twice, and the first base64 char is coerced into a
	// valid Go identifier. So we must be conservative.
	// Remember that base64 stores 6 bits per encoded byte.
	// The probability numbers are approximated.
	//
	//    length (base64) | length (bits) | collision probability
	//    -------------------------------------------------------
	//           4               24                   ~95%
	//           5               30                    ~4%
	//           6               36                 ~0.07%
	//           7               42                ~0.001%
	//           8               48              ~0.00001%
	//
	// We want collisions to be practically impossible, so we choose 8 to
	// end up with a chance of about 1 in a million even when a package has
	// thousands of obfuscated names.
	const hashLength = 8

	d := sha256.New()
	d.Write(salt)
	d.Write(opts.Seed)
	io.WriteString(d, name)
	sum := make([]byte, nameBase64.EncodedLen(d.Size()))
	nameBase64.Encode(sum, d.Sum(nil))
	sum = sum[:hashLength]

	// Even if we are hashing a package path, we still want the result to be
	// a valid identifier, since we'll use it as the package name too.
	if isDigit(sum[0]) {
		// Turn "3foo" into "Dfoo".
		// Similar to toLower, since uppercase letters go after digits
		// in the ASCII table.
		sum[0] += 'A' - '0'
	}
	// Keep the result equally exported or not, if it was an identifier.
	if !token.IsIdentifier(name) {
		return string(sum)
	}
	if token.IsExported(name) {
		if sum[0] == '_' {
			// Turn "_foo" into "Zfoo".
			sum[0] = 'Z'
		} else if isLower(sum[0]) {
			// Turn "afoo" into "Afoo".
			sum[0] = toUpper(sum[0])
		}
	} else {
		if isUpper(sum[0]) {
			// Turn "Afoo" into "afoo".
			sum[0] = toLower(sum[0])
		}
	}
	return string(sum)
}
