// NOTE(garble): bundled as of golang.org/x/tools v0.49.0; see CLAUDE.md.

//lint:file-ignore ST1012 NOTE(garble): err var names get a new prefix

package main

import (
	"errors"
	"fmt"
	"go/types"
	"os"
	"strings"
)

// NOTE(garble): from golang.org/x/tools/go/types/typeutil.

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
	return typeutil_hasher{
		inGenericSig: false,
		typeParamIDs: make(map[*types.TypeParam]uint32),
	}.hash(t)
}

// hasher holds the state of a single Hash traversal: whether we are
// inside the signature of a generic function; this is used to
// optimize [hasher.hashTypeParam].
//
// NOTE(garble): typeParamIDs assigns deterministic traversal-local IDs to free
// type params, making hashes stable across alpha-renaming in equivalent
// generic contexts.
type typeutil_hasher struct {
	inGenericSig bool
	typeParamIDs map[*types.TypeParam]uint32
}

// hashString computes the Fowler–Noll–Vo hash of s.
func typeutil_hashString(s string) uint32 {
	var h uint32
	for i := 0; i < len(s); i++ {
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
			// NOTE(garble): we must not hash struct field tags, as they do not affect type identity.
			// hash += typeutil_hashString(t.Tag(i))
			//
			// NOTE(garble): unlike upstream, we fold in the field position rather
			// than the field type, as this salt obfuscates field names (see
			// hashWithStruct) and must be stable across instantiations: an anonymous
			// struct{F Q} returned by a generic function has no origin to recover Q,
			// so a consuming package only sees an instantiation such as struct{F int}.
			// Positions still keep reordered structs distinct without the type.
			// hash += h.hash(f.Type())
			hash += (1 + uint32(i)) * typeutil_hashString(f.Name()) // (ignore f.Pkg)
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
		if n := tparams.Len(); n > 0 {
			h.inGenericSig = true // affects constraints, params, and results

			for i := range n {
				tparam := tparams.At(i)
				hash += 7 * h.hash(tparam.Constraint())
			}
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
		for targ := range targs.Types() {
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
	// NOTE(garble): unlike upstream, which hashes the type name of a free
	// TypeParam, we assign it a traversal-local ID; see below.
	//
	// When we are outside a generic function signature, free TypeParams can
	// come from equivalent generic contexts that only differ by alpha-renaming
	// (e.g. T vs newT). Object identity/name would make those hash differently.
	//
	// We therefore assign each free TypeParam a traversal-local canonical ID.
	// We intentionally keep the same "9173 + 3*x" shape used elsewhere for
	// TypeParams, only swapping x from Index/object-derived info to this ID.
	if !h.inGenericSig {
		// Outside generic function signatures, object identity depends on the
		// specific binder and does not survive alpha-renaming between equivalent
		// type expressions (e.g. T vs newT). Use a deterministic traversal-local
		// ID to keep hashes stable in those equivalent cases.
		if id, ok := h.typeParamIDs[t]; ok {
			// 9173 marks "type param", 3*id matches this hasher's mixing style.
			return 9173 + 3*id
		}
		id := uint32(len(h.typeParamIDs))
		h.typeParamIDs[t] = id
		return 9173 + 3*id
	}
	return 9173 + 3*uint32(t.Index())
}

// hashTypeName hashes the pointer of tname.
func (typeutil_hasher) hashTypeName(tname *types.TypeName) uint32 {
	// NOTE(garble): we must not hash any pointers,
	// as garble is a toolexec tool so by nature it uses multiple processes.
	return typeutil_hashString(tname.Name())
	// Since types.Identical uses == to compare TypeNames,
	// the Hash function uses maphash.Comparable.
	// hash := maphash.Comparable(typeutil_theSeed, tname)
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

// NOTE(garble): from golang.org/x/tools/internal/typeparams.

const typeparams_debug = false

var typeparams_ErrEmptyTypeSet = errors.New("empty type set")

// InterfaceTermSet computes the normalized terms for a constraint interface,
// returning an error if the term set cannot be computed or is empty. In the
// latter case, the error will be ErrEmptyTypeSet.
//
// See the documentation of StructuralTerms for more information on
// normalization.
func typeparams_InterfaceTermSet(iface *types.Interface) ([]*types.Term, error) {
	return typeparams_computeTermSet(iface)
}

// UnionTermSet computes the normalized terms for a union, returning an error
// if the term set cannot be computed or is empty. In the latter case, the
// error will be ErrEmptyTypeSet.
//
// See the documentation of StructuralTerms for more information on
// normalization.
func typeparams_UnionTermSet(union *types.Union) ([]*types.Term, error) {
	return typeparams_computeTermSet(union)
}

func typeparams_computeTermSet(typ types.Type) ([]*types.Term, error) {
	tset, err := typeparams_computeTermSetInternal(typ, make(map[types.Type]*typeparams_termSet), 0)
	if err != nil {
		return nil, err
	}
	if tset.terms.isEmpty() {
		return nil, typeparams_ErrEmptyTypeSet
	}
	if tset.terms.isAll() {
		return nil, nil
	}
	var terms []*types.Term
	for _, term := range tset.terms {
		terms = append(terms, types.NewTerm(term.tilde, term.typ))
	}
	return terms, nil
}

// A termSet holds the normalized set of terms for a given type.
//
// The name termSet is intentionally distinct from 'type set': a type set is
// all types that implement a type (and includes method restrictions), whereas
// a term set just represents the structural restrictions on a type.
type typeparams_termSet struct {
	complete bool
	terms    typeparams_termlist
}

func typeparams_indentf(depth int, format string, args ...any) {
	fmt.Fprintf(os.Stderr, strings.Repeat(".", depth)+format+"\n", args...)
}

func typeparams_computeTermSetInternal(t types.Type, seen map[types.Type]*typeparams_termSet, depth int) (res *typeparams_termSet, err error) {
	if t == nil {
		panic("nil type")
	}

	if typeparams_debug {
		typeparams_indentf(depth, "%s", t.String())
		defer func() {
			if err != nil {
				typeparams_indentf(depth, "=> %s", err)
			} else {
				typeparams_indentf(depth, "=> %s", res.terms.String())
			}
		}()
	}

	const maxTermCount = 100
	if tset, ok := seen[t]; ok {
		if !tset.complete {
			return nil, fmt.Errorf("cycle detected in the declaration of %s", t)
		}
		return tset, nil
	}

	// Mark the current type as seen to avoid infinite recursion.
	tset := new(typeparams_termSet)
	defer func() {
		tset.complete = true
	}()
	seen[t] = tset

	switch u := t.Underlying().(type) {
	case *types.Interface:
		// The term set of an interface is the intersection of the term sets of its
		// embedded types.
		tset.terms = typeparams_allTermlist
		for embedded := range u.EmbeddedTypes() {
			if _, ok := embedded.Underlying().(*types.TypeParam); ok {
				return nil, fmt.Errorf("invalid embedded type %T", embedded)
			}
			tset2, err := typeparams_computeTermSetInternal(embedded, seen, depth+1)
			if err != nil {
				return nil, err
			}
			tset.terms = tset.terms.intersect(tset2.terms)
		}
	case *types.Union:
		// The term set of a union is the union of term sets of its terms.
		tset.terms = nil
		for t := range u.Terms() {
			var terms typeparams_termlist
			switch t.Type().Underlying().(type) {
			case *types.Interface:
				tset2, err := typeparams_computeTermSetInternal(t.Type(), seen, depth+1)
				if err != nil {
					return nil, err
				}
				terms = tset2.terms
			case *types.TypeParam, *types.Union:
				// A stand-alone type parameter or union is not permitted as union
				// term.
				return nil, fmt.Errorf("invalid union term %T", t)
			default:
				if t.Type() == types.Typ[types.Invalid] {
					continue
				}
				terms = typeparams_termlist{{t.Tilde(), t.Type()}}
			}
			tset.terms = tset.terms.union(terms)
			if len(tset.terms) > maxTermCount {
				return nil, fmt.Errorf("exceeded max term count %d", maxTermCount)
			}
		}
	case *types.TypeParam:
		panic("unreachable")
	default:
		// For all other types, the term set is just a single non-tilde term
		// holding the type itself.
		if u != types.Typ[types.Invalid] {
			tset.terms = typeparams_termlist{{false, t}}
		}
	}
	return tset, nil
}

// under is a facade for the go/types internal function of the same name. It is
// used by typeterm.go.
func typeparams_under(t types.Type) types.Type {
	return t.Underlying()
}

// A termlist represents the type set represented by the union
// t1 ∪ y2 ∪ ... tn of the type sets of the terms t1 to tn.
// A termlist is in normal form if all terms are disjoint.
// termlist operations don't require the operands to be in
// normal form.
type typeparams_termlist []*typeparams_term

// allTermlist represents the set of all types.
// It is in normal form.

// allTermlist represents the set of all types.
// It is in normal form.
var typeparams_allTermlist = typeparams_termlist{new(typeparams_term)}

// termSep is the separator used between individual terms.
const typeparams_termSep = " | "

// String prints the termlist exactly (without normalization).
func (xl typeparams_termlist) String() string {
	if len(xl) == 0 {
		return "∅"
	}
	var buf strings.Builder
	for i, x := range xl {
		if i > 0 {
			buf.WriteString(typeparams_termSep)
		}
		buf.WriteString(x.String())
	}
	return buf.String()
}

// isEmpty reports whether the termlist xl represents the empty set of types.
func (xl typeparams_termlist) isEmpty() bool {
	// If there's a non-nil term, the entire list is not empty.
	// If the termlist is in normal form, this requires at most
	// one iteration.
	for _, x := range xl {
		if x != nil {
			return false
		}
	}
	return true
}

// isAll reports whether the termlist xl represents the set of all types.
func (xl typeparams_termlist) isAll() bool {
	// If there's a 𝓤 term, the entire list is 𝓤.
	// If the termlist is in normal form, this requires at most
	// one iteration.
	for _, x := range xl {
		if x != nil && x.typ == nil {
			return true
		}
	}
	return false
}

// norm returns the normal form of xl.
func (xl typeparams_termlist) norm() typeparams_termlist {
	// Quadratic algorithm, but good enough for now.
	// TODO(gri) fix asymptotic performance
	used := make([]bool, len(xl))
	var rl typeparams_termlist
	for i, xi := range xl {
		if xi == nil || used[i] {
			continue
		}
		for j := i + 1; j < len(xl); j++ {
			xj := xl[j]
			if xj == nil || used[j] {
				continue
			}
			if u1, u2 := xi.union(xj); u2 == nil {
				// If we encounter a 𝓤 term, the entire list is 𝓤.
				// Exit early.
				// (Note that this is not just an optimization;
				// if we continue, we may end up with a 𝓤 term
				// and other terms and the result would not be
				// in normal form.)
				if u1.typ == nil {
					return typeparams_allTermlist
				}
				xi = u1
				used[j] = true // xj is now unioned into xi - ignore it in future iterations
			}
		}
		rl = append(rl, xi)
	}
	return rl
}

// union returns the union xl ∪ yl.
func (xl typeparams_termlist) union(yl typeparams_termlist) typeparams_termlist {
	return append(xl, yl...).norm()
}

// intersect returns the intersection xl ∩ yl.
func (xl typeparams_termlist) intersect(yl typeparams_termlist) typeparams_termlist {
	if xl.isEmpty() || yl.isEmpty() {
		return nil
	}

	// Quadratic algorithm, but good enough for now.
	// TODO(gri) fix asymptotic performance
	var rl typeparams_termlist
	for _, x := range xl {
		for _, y := range yl {
			if r := x.intersect(y); r != nil {
				rl = append(rl, r)
			}
		}
	}
	return rl.norm()
}

// A term describes elementary type sets:
//
//	 ∅:  (*term)(nil)     == ∅                      // set of no types (empty set)
//	 𝓤:  &term{}          == 𝓤                      // set of all types (𝓤niverse)
//	 T:  &term{false, T}  == {T}                    // set of type T
//	~t:  &term{true, t}   == {t' | under(t') == t}  // set of types with underlying type t
type typeparams_term struct {
	tilde bool // valid if typ != nil
	typ   types.Type
}

func (x *typeparams_term) String() string {
	switch {
	case x == nil:
		return "∅"
	case x.typ == nil:
		return "𝓤"
	case x.tilde:
		return "~" + x.typ.String()
	default:
		return x.typ.String()
	}
}

// union returns the union x ∪ y: zero, one, or two non-nil terms.
func (x *typeparams_term) union(y *typeparams_term) (_, _ *typeparams_term) {
	// easy cases
	switch {
	case x == nil && y == nil:
		return nil, nil // ∅ ∪ ∅ == ∅
	case x == nil:
		return y, nil // ∅ ∪ y == y
	case y == nil:
		return x, nil // x ∪ ∅ == x
	case x.typ == nil:
		return x, nil // 𝓤 ∪ y == 𝓤
	case y.typ == nil:
		return y, nil // x ∪ 𝓤 == 𝓤
	}
	// ∅ ⊂ x, y ⊂ 𝓤

	if x.disjoint(y) {
		return x, y // x ∪ y == (x, y) if x ∩ y == ∅
	}
	// x.typ == y.typ

	// ~t ∪ ~t == ~t
	// ~t ∪  T == ~t
	//  T ∪ ~t == ~t
	//  T ∪  T ==  T
	if x.tilde || !y.tilde {
		return x, nil
	}
	return y, nil
}

// intersect returns the intersection x ∩ y.
func (x *typeparams_term) intersect(y *typeparams_term) *typeparams_term {
	// easy cases
	switch {
	case x == nil || y == nil:
		return nil // ∅ ∩ y == ∅ and ∩ ∅ == ∅
	case x.typ == nil:
		return y // 𝓤 ∩ y == y
	case y.typ == nil:
		return x // x ∩ 𝓤 == x
	}
	// ∅ ⊂ x, y ⊂ 𝓤

	if x.disjoint(y) {
		return nil // x ∩ y == ∅ if x ∩ y == ∅
	}
	// x.typ == y.typ

	// ~t ∩ ~t == ~t
	// ~t ∩  T ==  T
	//  T ∩ ~t ==  T
	//  T ∩  T ==  T
	if !x.tilde || y.tilde {
		return x
	}
	return y
}

// disjoint reports whether x ∩ y == ∅.
// x.typ and y.typ must not be nil.
func (x *typeparams_term) disjoint(y *typeparams_term) bool {
	if typeparams_debug && (x.typ == nil || y.typ == nil) {
		panic("invalid argument(s)")
	}
	ux := x.typ
	if y.tilde {
		ux = typeparams_under(ux)
	}
	uy := y.typ
	if x.tilde {
		uy = typeparams_under(uy)
	}
	return !types.Identical(ux, uy)
}
