package main

// The "name" internal/abi passes to this function doesn't have to be a simple "someName",
// it can also be for function names like "*pkgName.FuncName" (obfuscated)
// or for structs the entire struct definition, like
//
//	*struct { AQ45rr68K string; ipq5aQSIqN string; hNfiW5O5LVq struct { gPTbGR00hu string } }
//
// Therefore all obfuscated names which occur within name need to be replaced with their original equivalents.
// The code below does a more efficient version of:
//
//	func _originalNames(name string) string {
//		for _, pair := range _originalNamePairs {
//			name = strings.ReplaceAll(name, pair[0], pair[1])
//		}
//		return name
//	}
//
// The linknames below are only turned on when the code is injected,
// so that we can test and benchmark this code normally.

// Injected code below this line.

// Each pair is the obfuscated and then the real name.
// The pairs are sorted by obfuscated name, lexicographically.
var _originalNamePairs = []string{}

var _originalNamesReplacer *_genericReplacer

//disabledgo:linkname _originalNamesInit internal/abi._originalNamesInit
func _originalNamesInit() {
	_originalNamesReplacer = _makeGenericReplacer(_originalNamePairs)
}

//disabledgo:linkname _originalNames internal/abi._originalNames
func _originalNames(name string) string {
	return _originalNamesReplacer.Replace(name)
}

// -- Lifted from internal/stringslite --

func _hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[0:len(prefix)] == prefix
}

// -- Lifted from strings as of Go 1.23 --
//
// With minor modifications to avoid type assertions,
// as any reflection in internal/abi causes a recursive call to the runtime
// which locks up the entire runtime. Moreover, we can't import strings.
//
// Updating the code below should not be necessary in general,
// unless upstream Go makes significant improvements to this replacer implementation.

// _trieNode is a node in a lookup trie for prioritized key/value pairs. Keys
// and values may be empty. For example, the trie containing keys "ax", "ay",
// "bcbc", "x" and "xy" could have eight nodes:
//
//	n0  -
//	n1  a-
//	n2  .x+
//	n3  .y+
//	n4  b-
//	n5  .cbc+
//	n6  x+
//	n7  .y+
//
// n0 is the root node, and its children are n1, n4 and n6; n1's children are
// n2 and n3; n4's child is n5; n6's child is n7. Nodes n0, n1 and n4 (marked
// with a trailing "-") are partial keys, and nodes n2, n3, n5, n6 and n7
// (marked with a trailing "+") are complete keys.
type _trieNode struct {
	// value is the value of the trie node's key/value pair. It is empty if
	// this node is not a complete key.
	value string
	// priority is the priority (higher is more important) of the trie node's
	// key/value pair; keys are not necessarily matched shortest- or longest-
	// first. Priority is positive if this node is a complete key, and zero
	// otherwise. In the example above, positive/zero priorities are marked
	// with a trailing "+" or "-".
	priority int

	// A trie node may have zero, one or more child nodes:
	//  * if the remaining fields are zero, there are no children.
	//  * if prefix and next are non-zero, there is one child in next.
	//  * if table is non-zero, it defines all the children.
	//
	// Prefixes are preferred over tables when there is one child, but the
	// root node always uses a table for lookup efficiency.

	// prefix is the difference in keys between this trie node and the next.
	// In the example above, node n4 has prefix "cbc" and n4's next node is n5.
	// Node n5 has no children and so has zero prefix, next and table fields.
	prefix string
	next   *_trieNode

	// table is a lookup table indexed by the next byte in the key, after
	// remapping that byte through _genericReplacer.mapping to create a dense
	// index. In the example above, the keys only use 'a', 'b', 'c', 'x' and
	// 'y', which remap to 0, 1, 2, 3 and 4. All other bytes remap to 5, and
	// _genericReplacer.tableSize will be 5. Node n0's table will be
	// []*_trieNode{ 0:n1, 1:n4, 3:n6 }, where the 0, 1 and 3 are the remapped
	// 'a', 'b' and 'x'.
	table []*_trieNode
}

func (t *_trieNode) add(key, val string, priority int, r *_genericReplacer) {
	if key == "" {
		if t.priority == 0 {
			t.value = val
			t.priority = priority
		}
		return
	}

	if t.prefix != "" {
		var n int // length of the longest common prefix
		for ; n < len(t.prefix) && n < len(key); n++ {
			if t.prefix[n] != key[n] {
				break
			}
		}
		if n == len(t.prefix) {
			t.next.add(key[n:], val, priority, r)
		} else if n == 0 {
			var prefixNode *_trieNode
			if len(t.prefix) == 1 {
				prefixNode = t.next
			} else {
				prefixNode = &_trieNode{
					prefix: t.prefix[1:],
					next:   t.next,
				}
			}
			keyNode := new(_trieNode)
			t.table = make([]*_trieNode, r.tableSize)
			t.table[r.mapping[t.prefix[0]]] = prefixNode
			t.table[r.mapping[key[0]]] = keyNode
			t.prefix = ""
			t.next = nil
			keyNode.add(key[1:], val, priority, r)
		} else {
			// Insert new node after the common section of the prefix.
			next := &_trieNode{
				prefix: t.prefix[n:],
				next:   t.next,
			}
			t.prefix = t.prefix[:n]
			t.next = next
			next.add(key[n:], val, priority, r)
		}
	} else if t.table != nil {
		// Insert into existing table.
		m := r.mapping[key[0]]
		if t.table[m] == nil {
			t.table[m] = new(_trieNode)
		}
		t.table[m].add(key[1:], val, priority, r)
	} else {
		t.prefix = key
		t.next = new(_trieNode)
		t.next.add("", val, priority, r)
	}
}

func (r *_genericReplacer) lookup(s string, ignoreRoot bool) (val string, keylen int, found bool) {
	// Iterate down the trie to the end, and grab the value and keylen with
	// the highest priority.
	bestPriority := 0
	node := &r.root
	n := 0
	for node != nil {
		if node.priority > bestPriority && !(ignoreRoot && node == &r.root) {
			bestPriority = node.priority
			val = node.value
			keylen = n
			found = true
		}

		if s == "" {
			break
		}
		if node.table != nil {
			index := r.mapping[s[0]]
			if int(index) == r.tableSize {
				break
			}
			node = node.table[index]
			s = s[1:]
			n++
		} else if node.prefix != "" && _hasPrefix(s, node.prefix) {
			n += len(node.prefix)
			s = s[len(node.prefix):]
			node = node.next
		} else {
			break
		}
	}
	return
}

type _genericReplacer struct {
	root _trieNode
	// tableSize is the size of a trie node's lookup table. It is the number
	// of unique key bytes.
	tableSize int
	// mapping maps from key bytes to a dense index for _trieNode.table.
	mapping [256]byte
}

func _makeGenericReplacer(oldnew []string) *_genericReplacer {
	r := new(_genericReplacer)
	// Find each byte used, then assign them each an index.
	for i := 0; i < len(oldnew); i += 2 {
		key := oldnew[i]
		for j := 0; j < len(key); j++ {
			r.mapping[key[j]] = 1
		}
	}

	for _, b := range r.mapping {
		r.tableSize += int(b)
	}

	var index byte
	for i, b := range r.mapping {
		if b == 0 {
			r.mapping[i] = byte(r.tableSize)
		} else {
			r.mapping[i] = index
			index++
		}
	}
	// Find each byte used, then assign them each an index.
	r.root.table = make([]*_trieNode, r.tableSize)

	for i := 0; i < len(oldnew); i += 2 {
		r.root.add(oldnew[i], oldnew[i+1], len(oldnew)-i, r)
	}
	return r
}

func (r *_genericReplacer) Replace(s string) string {
	dst := make([]byte, 0, len(s))
	var last int
	var prevMatchEmpty bool
	for i := 0; i <= len(s); {
		// Fast path: s[i] is not a prefix of any pattern.
		if i != len(s) && r.root.priority == 0 {
			index := int(r.mapping[s[i]])
			if index == r.tableSize || r.root.table[index] == nil {
				i++
				continue
			}
		}

		// Ignore the empty match iff the previous loop found the empty match.
		val, keylen, match := r.lookup(s[i:], prevMatchEmpty)
		prevMatchEmpty = match && keylen == 0
		if match {
			dst = append(dst, s[last:i]...)
			dst = append(dst, val...)
			i += keylen
			last = i
			continue
		}
		i++
	}
	if last != len(s) {
		dst = append(dst, s[last:]...)
	}
	return string(dst)
}
