// Copyright (c) 2019, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"go/token"
	"go/types"
	"io"
	"os/exec"
	"strings"

	"mvdan.cc/garble/internal/literals"
)

const buildIDSeparator = "/"

// splitActionID returns the action ID half of a build ID, the first hash.
func splitActionID(buildID string) string {
	return buildID[:strings.Index(buildID, buildIDSeparator)]
}

// splitContentID returns the content ID half of a build ID, the last hash.
func splitContentID(buildID string) string {
	return buildID[strings.LastIndex(buildID, buildIDSeparator)+1:]
}

// buildIDHashLength is the number of bytes each build ID hash takes,
// such as an action ID or a content ID.
const buildIDHashLength = 15

// decodeBuildIDHash decodes a build ID hash in base64, just like cmd/go does.
func decodeBuildIDHash(str string) []byte {
	h, err := base64.RawURLEncoding.DecodeString(str)
	if err != nil {
		panic(fmt.Sprintf("invalid hash %q: %v", str, err))
	}
	if len(h) != buildIDHashLength {
		panic(fmt.Sprintf("decodeBuildIDHash expects to result in a hash of length %d, got %d", buildIDHashLength, len(h)))
	}
	return h
}

// encodeBuildIDHash encodes a build ID hash in base64, just like cmd/go does.
func encodeBuildIDHash(h [sha256.Size]byte) string {
	return base64.RawURLEncoding.EncodeToString(h[:buildIDHashLength])
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
		toolID = decodeBuildIDHash(splitContentID(f[len(f)-1]))
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
	// the other hashes such as the action ID are not necessary, since the
	// only reader here is cmd/go and it only consumes the content ID.
	fmt.Printf("%s +garble buildID=_/_/_/%s\n", line, encodeBuildIDHash(contentID))
	return nil
}

var (
	hasher    = sha256.New()
	sumBuffer [sha256.Size]byte
)

// addGarbleToHash takes some arbitrary input bytes,
// typically a hash such as an action ID or a content ID,
// and returns a new hash which also contains garble's own deterministic inputs.
//
// This includes garble's own version, obtained via its own binary's content ID,
// as well as any other options which affect a build, such as GOGARBLE and -tiny.
func addGarbleToHash(inputHash []byte) [sha256.Size]byte {
	// Join the two content IDs together into a single base64-encoded sha256
	// sum. This includes the original tool's content ID, and garble's own
	// content ID.
	hasher.Reset()
	hasher.Write(inputHash)
	if len(sharedCache.BinaryContentID) == 0 {
		panic("missing binary content ID")
	}
	hasher.Write(sharedCache.BinaryContentID)

	// We also need to add the selected options to the full version string,
	// because all of them result in different output. We use spaces to
	// separate the env vars and flags, to reduce the chances of collisions.
	fmt.Fprintf(hasher, " GOGARBLE=%s", sharedCache.GOGARBLE)
	appendFlags(hasher, true)
	// addGarbleToHash returns the sum buffer, so we need a new copy.
	// Otherwise the next use of the global sumBuffer would conflict.
	var sumBuffer [sha256.Size]byte
	hasher.Sum(sumBuffer[:0])
	return sumBuffer
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
	if flagControlFlow && forBuildHash {
		io.WriteString(w, " -ctrlflow")
	}
	if literals.TestObfuscator != "" && forBuildHash {
		io.WriteString(w, literals.TestObfuscator)
	}
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
	// This means we can use base64's URL encoding, minus '-',
	// which is later replaced with a duplicate 'a'.
	// Such a lossy encoding is fine, since we never decode hashes.
	// We don't need padding either, as we take a short prefix anyway.
	nameBase64 = base64.URLEncoding.WithPadding(base64.NoPadding)

	b64NameBuffer [12]byte // nameBase64.EncodedLen(neededSumBytes) = 12
)

// These funcs mimic the unicode package API, but byte-based since we know
// base64 is all ASCII.

func isDigit(b byte) bool { return '0' <= b && b <= '9' }
func isLower(b byte) bool { return 'a' <= b && b <= 'z' }
func isUpper(b byte) bool { return 'A' <= b && b <= 'Z' }
func toLower(b byte) byte { return b + ('a' - 'A') }
func toUpper(b byte) byte { return b - ('a' - 'A') }

func runtimeHashWithCustomSalt(salt []byte) uint32 {
	hasher.Reset()
	if !flagSeed.present() {
		hasher.Write(sharedCache.ListedPackages["runtime"].GarbleActionID[:])
	} else {
		hasher.Write(flagSeed.bytes)
	}
	hasher.Write(salt)
	sum := hasher.Sum(sumBuffer[:0])
	return binary.LittleEndian.Uint32(sum)
}

// magicValue returns random magic value based
// on user specified seed or the runtime package's GarbleActionID.
func magicValue() uint32 {
	return runtimeHashWithCustomSalt([]byte("magic"))
}

// entryOffKey returns random entry offset key
// on user specified seed or the runtime package's GarbleActionID.
func entryOffKey() uint32 {
	return runtimeHashWithCustomSalt([]byte("entryOffKey"))
}

func hashWithPackage(pkg *listedPackage, name string) string {
	// If the user provided us with an obfuscation seed,
	// we use that with the package import path directly..
	// Otherwise, we use GarbleActionID as a fallback salt.
	if !flagSeed.present() {
		return hashWithCustomSalt(pkg.GarbleActionID[:], name)
	}
	// Use a separator at the end of ImportPath as a salt,
	// to ensure that "pkgfoo.bar" and "pkg.foobar" don't both hash
	// as the same string "pkgfoobar".
	return hashWithCustomSalt([]byte(pkg.ImportPath+"|"), name)
}

// stripStructTags takes the bytes produced by [types.WriteType]
// and removes any struct tags in-place, such as rewriting
//
//	struct{Foo int; Bar string "json:\"bar\""}
//
// into
//
//	struct{Foo int; Bar string}
//
// Note that, unlike most Go source, WriteType uses double quotes for tags.
//
// Reusing WriteType does require a second pass over its output here,
// which we could save by implementing our own modified version of WriteType.
// However, that would be a significant amount of code to maintain.
func stripStructTags(p []byte) []byte {
	i := 0
	for i < len(p) {
		b := p[i]
		start := i - 1 // a struct tag is preceded by a space
		i++
		if b != '"' {
			continue
		}
		// Find the closing double quote, skipping over escaped characters.
		// Note that we should probably iterate over runes and not bytes,
		// but this byte implementation is probably good enough in practice.
		for {
			b = p[i]
			i++
			if b == '\\' {
				i++
			} else if b == '"' {
				break
			}
		}
		end := i
		// Remove the bytes between start and end,
		// and reset i to start, since we just shortened p.
		p = append(p[:start], p[end:]...)
		i = start
	}
	return p
}

var typeIdentityBuf bytes.Buffer

// hashWithStruct is separate from hashWithPackage since Go
// allows converting between struct types across packages.
// Hashing struct field names differently between packages would break that.
//
// We hash field names with the identity struct type as a salt
// so that the same field name used in different struct types is obfuscated differently.
// Note that "identity" means omitting struct tags since conversions ignore them.
func hashWithStruct(strct *types.Struct, field *types.Var) string {
	typeIdentityBuf.Reset()
	types.WriteType(&typeIdentityBuf, strct, nil)
	salt := stripStructTags(typeIdentityBuf.Bytes())

	// If the user provided us with an obfuscation seed,
	// we only use the identity struct type as a salt.
	// Otherwise, we add garble's own inputs to the salt as a fallback.
	if !flagSeed.present() {
		withGarbleHash := addGarbleToHash(salt)
		salt = withGarbleHash[:]
	}
	return hashWithCustomSalt(salt, field.Name())
}

// minHashLength and maxHashLength define the range for the number of base64
// characters to use for the final hashed name.
//
// minHashLength needs to be long enough to realistically avoid hash collisions,
// but maxHashLength should be short enough to not bloat binary sizes.
// The namespace for collisions is generally a single package, since
// that's where most hashed names are namespaced to.
//
// Using a "hash collision" formula, and taking a generous estimate of a
// package having 10k names, we get the following probabilities.
// Most packages will have far fewer names, but some packages are huge,
// especially generated ones.
//
// We also have slightly fewer bits in practice, since the base64
// charset has 'z' twice, and the first base64 char is coerced into a
// valid Go identifier. So we must be conservative.
// Remember that base64 stores 6 bits per encoded byte.
// The probability numbers are approximated.
//
//	length (base64) | length (bits) | collision probability
//	-------------------------------------------------------
//	       4               24                   ~95%
//	       5               30                    ~4%
//	       6               36                 ~0.07%
//	       7               42                ~0.001%
//	       8               48              ~0.00001%
//
// We want collisions to be practically impossible, so the hashed names end up
// with lengths evenly distributed between 6 and 12. Naively, this results in an
// average length of 9, which has a chance well below 1 in a million even when a
// package has thousands of obfuscated names.
//
// These numbers are also chosen to keep obfuscated binary sizes reasonable.
// For example, increasing the average length of 9 by 1 results in roughly a 1%
// increase in binary sizes.
const (
	minHashLength = 6
	maxHashLength = 12

	// At most we'll need maxHashLength base64 characters,
	// so 9 checksum bytes are enough for that purpose,
	// which is nameBase64.DecodedLen(12) being rounded up.
	neededSumBytes = 9
)

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

	hasher.Reset()
	hasher.Write(salt)
	hasher.Write(flagSeed.bytes)
	io.WriteString(hasher, name)
	sum := hasher.Sum(sumBuffer[:0])

	// The byte after neededSumBytes is never used as part of the name,
	// but it is still deterministic and hard to predict,
	// so it provides us with useful randomness between 0 and 255.
	// We want the number to be between 0 and hashLenthRange-1 as well,
	// so we use a remainder operation.
	hashLengthRandomness := sum[neededSumBytes] % ((maxHashLength - minHashLength) + 1)
	hashLength := minHashLength + hashLengthRandomness

	nameBase64.Encode(b64NameBuffer[:], sum[:neededSumBytes])
	b64Name := b64NameBuffer[:hashLength]

	// Even if we are hashing a package path, which is not an identifier,
	// we still want the result to be a valid identifier,
	// since we'll use it as the package name too.
	if isDigit(b64Name[0]) {
		// Turn "3foo" into "Dfoo".
		// Similar to toLower, since uppercase letters go after digits
		// in the ASCII table.
		b64Name[0] += 'A' - '0'
	}
	for i, b := range b64Name {
		if b == '-' { // URL encoding uses dashes, which aren't valid
			b64Name[i] = 'a'
		}
	}
	// Valid identifiers should stay exported or unexported.
	if token.IsIdentifier(name) {
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
	}
	return string(b64Name)
}
