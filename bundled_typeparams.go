// Originally bundled from golang.org/x/tools/internal/typeparams@v0.29.0,
// as it is used by x/tools/go/types/typeutil and is an internal package.

package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/types"
)

var errEmptyTypeSet = errors.New("empty type set")

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
		return nil, errEmptyTypeSet
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

func typeparams_computeTermSetInternal(t types.Type, seen map[types.Type]*typeparams_termSet, depth int) (res *typeparams_termSet, err error) {
	if t == nil {
		panic("nil type")
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
		for i := range u.NumEmbeddeds() {
			embedded := u.EmbeddedType(i)
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
		for i := range u.Len() {
			t := u.Term(i)
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

// String prints the termlist exactly (without normalization).
func (xl typeparams_termlist) String() string {
	if len(xl) == 0 {
		return "∅"
	}
	var buf bytes.Buffer
	for i, x := range xl {
		if i > 0 {
			buf.WriteString(" | ")
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
