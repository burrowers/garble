package main

// The "name" internal/abi passes to this function doesn't have to be a simple "someName"
// it can also be for function names:
// "*pkgName.FuncName" (obfuscated)
// or for structs the entire struct definition:
// "*struct { AQ45rr68K string; ipq5aQSIqN string; hNfiW5O5LVq struct { gPTbGR00hu string } }"
//
// Therefore all obfuscated names which occur within name need to be replaced with their "real" equivalents.
//
// The code below does a more efficient version of:
//
//	func _realName(name string) string {
//			for obfName, real := range _nameMap {
//				name = strings.ReplaceAll(name, obfName, real)
//			}
//
//			return name
//	}
//
// The linknames below are only turned on when the code is injected,
// so that we can test and benchmark this code normally.

// Injected code below this line.

//disabledgo:linkname _realName internal/abi._realName
func _realName(name string) string {
	if len(name) < minHashLength {
		// The name is too short to be obfuscated.
		return name
	}
	// We can stop once there aren't enough bytes to fit another obfuscated name.
	for i := 0; i <= len(name)-minHashLength; {
		switch name[i] {
		case ' ', '.', '*', '{', '}', '[', ']':
			// These characters never start an obfuscated name.
			i++
			continue
		}
		remLen := len(name[i:])
		found := false
		for _, pair := range _realNamePairs {
			obfName := pair[0]
			real := pair[1]
			keyLen := len(obfName)
			if remLen < keyLen {
				// Since the pairs are sorted from shortest to longest name,
				// we know that the rest of the pairs are at least just as long.
				break
			}
			if name[i:i+keyLen] == obfName {
				name = name[:i] + real + name[i+keyLen:]
				found = true
				i += len(real)
				break
			}
		}
		if !found {
			i++
		}
	}
	return name
}

// Each pair is the obfuscated and then the real name.
// The slice is sorted from shortest to longest obfuscated name.
var _realNamePairs = [][2]string{}
