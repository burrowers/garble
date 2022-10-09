// Copyright (c) 2019, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"go/token"
	"go/types"
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

var (
	hasher       = sha256.New()
	sumBuffer    [sha256.Size]byte
	b64SumBuffer [44]byte // base64's EncodedLen on sha256.Size (32) with no padding
)

// addGarbleToHash takes some arbitrary input bytes,
// typically a hash such as an action ID or a content ID,
// and returns a new hash which also contains garble's own deterministic inputs.
//
// This includes garble's own version, obtained via its own binary's content ID,
// as well as any other options which affect a build, such as GOGARBLE and -tiny.
func addGarbleToHash(inputHash []byte) []byte {
	// Join the two content IDs together into a single base64-encoded sha256
	// sum. This includes the original tool's content ID, and garble's own
	// content ID.
	hasher.Reset()
	hasher.Write(inputHash)
	if len(cache.BinaryContentID) == 0 {
		panic("missing binary content ID")
	}
	hasher.Write(cache.BinaryContentID)

	// We also need to add the selected options to the full version string,
	// because all of them result in different output. We use spaces to
	// separate the env vars and flags, to reduce the chances of collisions.
	if cache.GOGARBLE != "" {
		fmt.Fprintf(hasher, " GOGARBLE=%s", cache.GOGARBLE)
	}
	appendFlags(hasher, true)
	// addGarbleToHash returns the sum buffer, so we need a new copy.
	// Otherwise the next use of the global sumBuffer would conflict.
	sumBuffer := make([]byte, 0, sha256.Size)
	return hasher.Sum(sumBuffer)[:buildIDComponentLength]
}

// appendFlags writes garble's own flags to w in string form.
// Errors are ignored, as w is always a buffer or hasher.
// If forBuildHash is set, only the flags affecting a build are written.
func appendFlags(w io.Writer, forBuildHash bool) {
	if flagLiterals {
		io.WriteString(w, " -literals")
	}
	if flagTiny {
		io.WriteString(w, " -tiny")
	}
	if flagDebug && !forBuildHash {
		// -debug doesn't affect the build result at all,
		// so don't give it separate entries in the build cache.
		// If the user really wants to see debug info for already built deps,
		// they can use "go clean cache" or the "-a" build flag to rebuild.
		io.WriteString(w, " -debug")
	}
	if flagDebugDir != "" && !forBuildHash {
		// -debugdir is a bit special.
		//
		// When passing down flags via -toolexec,
		// we do want the actual flag value to be kept.
		//
		// For build hashes, we can skip the flag entirely,
		// as it doesn't affect obfuscation at all.
		//
		// TODO: in the future, we could avoid using the -a build flag
		// by using "-debugdir=yes" here, and caching the obfuscated source.
		// Incremental builds would recover the cached source
		// to repopulate the output directory if it was removed.
		io.WriteString(w, " -debugdir=")
		io.WriteString(w, flagDebugDir)
	}
	if flagSeed.present() {
		io.WriteString(w, " -seed=")
		io.WriteString(w, flagSeed.String())
	}
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

func hashWithPackage(pkg *listedPackage, name string) string {
	if !flagSeed.present() {
		return hashWithCustomSalt(pkg.GarbleActionID, name)
	}
	// Use a separator at the end of ImportPath as a salt,
	// to ensure that "pkgfoo.bar" and "pkg.foobar" don't both hash
	// as the same string "pkgfoobar".
	return hashWithCustomSalt([]byte(pkg.ImportPath+"|"), name)
}

func hashWithStruct(strct *types.Struct, fieldName string) string {
	// TODO: We should probably strip field tags here.
	// Do we need to do anything else to make a
	// struct type "canonical"?
	fieldsSalt := []byte(strct.String())
	if !flagSeed.present() {
		fieldsSalt = addGarbleToHash(fieldsSalt)
	}
	return hashWithCustomSalt(fieldsSalt, fieldName)
}

// hashWithCustomSalt returns a hashed version of name,
// including the provided salt as well as opts.Seed into the hash input.
//
// The result is always four bytes long. If the input was a valid identifier,
// the output remains equally exported or unexported. Note that this process is
// reproducible, but not reversible.
func hashWithCustomSalt(salt []byte, name string) string {
	if len(salt) == 0 {
		panic("hashWithCustomSalt: empty salt")
	}
	if name == "" {
		panic("hashWithCustomSalt: empty name")
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

	const minHashLength = 8
	const maxHashLength = 15
	const hashLengthRange = maxHashLength - minHashLength

	hasher.Reset()
	hasher.Write(salt)
	hasher.Write(flagSeed.bytes)
	io.WriteString(hasher, name)
	nameBase64.Encode(b64SumBuffer[:], hasher.Sum(sumBuffer[:0]))

	hashLengthRandomness := b64SumBuffer[len(b64SumBuffer)-2] % hashLengthRange
	hashLength := minHashLength + hashLengthRandomness
	b64Name := b64SumBuffer[:hashLength]

	// Even if we are hashing a package path, we still want the result to be
	// a valid identifier, since we'll use it as the package name too.
	if isDigit(b64Name[0]) {
		// Turn "3foo" into "Dfoo".
		// Similar to toLower, since uppercase letters go after digits
		// in the ASCII table.
		b64Name[0] += 'A' - '0'
	}
	// Keep the result equally exported or not, if it was an identifier.
	if !token.IsIdentifier(name) {
		return string(b64Name)
	}
	if token.IsExported(name) {
		if b64Name[0] == '_' {
			// Turn "_foo" into "Zfoo".
			b64Name[0] = 'Z'
		} else if isLower(b64Name[0]) {
			// Turn "afoo" into "Afoo".
			b64Name[0] = toUpper(b64Name[0])
		}
	} else {
		if isUpper(b64Name[0]) {
			// Turn "Afoo" into "afoo".
			b64Name[0] = toLower(b64Name[0])
		}
	}
	return string(b64Name)
}
