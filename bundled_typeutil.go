// Originally bundled from golang.org/x/tools/go/types/typeutil@v0.29.0.
// Edited to just keep the hasher API in place, removing the use of internal/typeparams,
// and removed the inclusion of struct field tags in the hasher.

package main

import (
	"fmt"
	"go/types"
)

// -- Hasher --

// hash returns the hash of type t.
// TODO(adonovan): replace by types.Hash when Go proposal #69420 is accepted.
func typeutil_hash(t types.Type) uint32 {
	return typeutil_theHasher.Hash(t)
}

// A Hasher provides a [Hasher.Hash] method to map a type to its hash value.
// Hashers are stateless, and all are equivalent.
type typeutil_Hasher struct{}

var typeutil_theHasher typeutil_Hasher

// Hash computes a hash value for the given type t such that
// Identical(t, t') => Hash(t) == Hash(t').
func (h typeutil_Hasher) Hash(t types.Type) uint32 {
	return typeutil_hasher{inGenericSig: false}.hash(t)
}

// hasher holds the state of a single Hash traversal: whether we are
// inside the signature of a generic function; this is used to
// optimize [hasher.hashTypeParam].
type typeutil_hasher struct{ inGenericSig bool }

// hashString computes the Fowler–Noll–Vo hash of s.
func typeutil_hashString(s string) uint32 {
	var h uint32
	for i := range len(s) {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

// hash computes the hash of t.
func (h typeutil_hasher) hash(t types.Type) uint32 {
	// See Identical for rationale.
	switch t := t.(type) {
	case *types.Basic:
		return uint32(t.Kind())

	case *types.Alias:
		return h.hash(types.Unalias(t))

	case *types.Array:
		return 9043 + 2*uint32(t.Len()) + 3*h.hash(t.Elem())

	case *types.Slice:
		return 9049 + 2*h.hash(t.Elem())

	case *types.Struct:
		var hash uint32 = 9059
		for i, n := 0, t.NumFields(); i < n; i++ {
			f := t.Field(i)
			if f.Anonymous() {
				hash += 8861
			}
			// NOTE: we must not hash struct field tags, as they do not affect type identity.
			// hash += typeutil_hashString(t.Tag(i))
			hash += typeutil_hashString(f.Name()) // (ignore f.Pkg)
			hash += h.hash(f.Type())
		}
		return hash

	case *types.Pointer:
		return 9067 + 2*h.hash(t.Elem())

	case *types.Signature:
		var hash uint32 = 9091
		if t.Variadic() {
			hash *= 8863
		}

		tparams := t.TypeParams()
		for i := range tparams.Len() {
			h.inGenericSig = true
			tparam := tparams.At(i)
			hash += 7 * h.hash(tparam.Constraint())
		}

		return hash + 3*h.hashTuple(t.Params()) + 5*h.hashTuple(t.Results())

	case *types.Union:
		return h.hashUnion(t)

	case *types.Interface:
		// Interfaces are identical if they have the same set of methods, with
		// identical names and types, and they have the same set of type
		// restrictions. See go/types.identical for more details.
		var hash uint32 = 9103

		// Hash methods.
		for i, n := 0, t.NumMethods(); i < n; i++ {
			// Method order is not significant.
			// Ignore m.Pkg().
			m := t.Method(i)
			// Use shallow hash on method signature to
			// avoid anonymous interface cycles.
			hash += 3*typeutil_hashString(m.Name()) + 5*h.shallowHash(m.Type())
		}

		// Hash type restrictions.
		terms, err := typeparams_InterfaceTermSet(t)
		// if err != nil t has invalid type restrictions.
		if err == nil {
			hash += h.hashTermSet(terms)
		}

		return hash

	case *types.Map:
		return 9109 + 2*h.hash(t.Key()) + 3*h.hash(t.Elem())

	case *types.Chan:
		return 9127 + 2*uint32(t.Dir()) + 3*h.hash(t.Elem())

	case *types.Named:
		hash := h.hashTypeName(t.Obj())
		targs := t.TypeArgs()
		for i := range targs.Len() {
			targ := targs.At(i)
			hash += 2 * h.hash(targ)
		}
		return hash

	case *types.TypeParam:
		return h.hashTypeParam(t)

	case *types.Tuple:
		return h.hashTuple(t)
	}

	panic(fmt.Sprintf("%T: %v", t, t))
}

func (h typeutil_hasher) hashTuple(tuple *types.Tuple) uint32 {
	// See go/types.identicalTypes for rationale.
	n := tuple.Len()
	hash := 9137 + 2*uint32(n)
	for i := range n {
		hash += 3 * h.hash(tuple.At(i).Type())
	}
	return hash
}

func (h typeutil_hasher) hashUnion(t *types.Union) uint32 {
	// Hash type restrictions.
	terms, err := typeparams_UnionTermSet(t)
	// if err != nil t has invalid type restrictions. Fall back on a non-zero
	// hash.
	if err != nil {
		return 9151
	}
	return h.hashTermSet(terms)
}

func (h typeutil_hasher) hashTermSet(terms []*types.Term) uint32 {
	hash := 9157 + 2*uint32(len(terms))
	for _, term := range terms {
		// term order is not significant.
		termHash := h.hash(term.Type())
		if term.Tilde() {
			termHash *= 9161
		}
		hash += 3 * termHash
	}
	return hash
}

// hashTypeParam returns the hash of a type parameter.
func (h typeutil_hasher) hashTypeParam(t *types.TypeParam) uint32 {
	// Within the signature of a generic function, TypeParams are
	// identical if they have the same index and constraint, so we
	// hash them based on index.
	//
	// When we are outside a generic function, free TypeParams are
	// identical iff they are the same object, so we can use a
	// more discriminating hash consistent with object identity.
	// This optimization saves [Map] about 4% when hashing all the
	// types.Info.Types in the forward closure of net/http.
	if !h.inGenericSig {
		// Optimization: outside a generic function signature,
		// use a more discrimating hash consistent with object identity.
		return h.hashTypeName(t.Obj())
	}
	return 9173 + 3*uint32(t.Index())
}

// hashTypeName hashes the pointer of tname.
func (typeutil_hasher) hashTypeName(tname *types.TypeName) uint32 {
	// NOTE: we must not hash any pointers, as garble is a toolexec tool
	// so by nature it uses multiple processes.
	return typeutil_hashString(tname.Name())
	// Since types.Identical uses == to compare TypeNames,
	// the Hash function uses maphash.Comparable.
	// TODO(adonovan): or will, when it becomes available in go1.24.
	// In the meantime we use the pointer's numeric value.
	//
	//   hash := maphash.Comparable(theSeed, tname)
	//
	// (Another approach would be to hash the name and package
	// path, and whether or not it is a package-level typename. It
	// is rare for a package to define multiple local types with
	// the same name.)
	// hash := uintptr(unsafe.Pointer(tname))
	// return uint32(hash ^ (hash >> 32))
}

// shallowHash computes a hash of t without looking at any of its
// element Types, to avoid potential anonymous cycles in the types of
// interface methods.
//
// When an unnamed non-empty interface type appears anywhere among the
// arguments or results of an interface method, there is a potential
// for endless recursion. Consider:
//
//	type X interface { m() []*interface { X } }
//
// The problem is that the Methods of the interface in m's result type
// include m itself; there is no mention of the named type X that
// might help us break the cycle.
// (See comment in go/types.identical, case *Interface, for more.)
func (h typeutil_hasher) shallowHash(t types.Type) uint32 {
	// t is the type of an interface method (Signature),
	// its params or results (Tuples), or their immediate
	// elements (mostly Slice, Pointer, Basic, Named),
	// so there's no need to optimize anything else.
	switch t := t.(type) {
	case *types.Alias:
		return h.shallowHash(types.Unalias(t))

	case *types.Signature:
		var hash uint32 = 604171
		if t.Variadic() {
			hash *= 971767
		}
		// The Signature/Tuple recursion is always finite
		// and invariably shallow.
		return hash + 1062599*h.shallowHash(t.Params()) + 1282529*h.shallowHash(t.Results())

	case *types.Tuple:
		n := t.Len()
		hash := 9137 + 2*uint32(n)
		for i := range n {
			hash += 53471161 * h.shallowHash(t.At(i).Type())
		}
		return hash

	case *types.Basic:
		return 45212177 * uint32(t.Kind())

	case *types.Array:
		return 1524181 + 2*uint32(t.Len())

	case *types.Slice:
		return 2690201

	case *types.Struct:
		return 3326489

	case *types.Pointer:
		return 4393139

	case *types.Union:
		return 562448657

	case *types.Interface:
		return 2124679 // no recursion here

	case *types.Map:
		return 9109

	case *types.Chan:
		return 9127

	case *types.Named:
		return h.hashTypeName(t.Obj())

	case *types.TypeParam:
		return h.hashTypeParam(t)
	}
	panic(fmt.Sprintf("shallowHash: %T: %v", t, t))
}
