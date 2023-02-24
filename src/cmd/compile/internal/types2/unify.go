// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements type unification.
//
// Type unification attempts to make two types x and y structurally
// identical by determining the types for a given list of (bound)
// type parameters which may occur within x and y. If x and y are
// are structurally different (say []T vs chan T), or conflicting
// types are determined for type parameters, unification fails.
// If unification succeeds, as a side-effect, the types of the
// bound type parameters may be determined.
//
// Unification typically requires multiple calls u.unify(x, y) to
// a given unifier u, with various combinations of types x and y.
// In each call, additional type parameter types may be determined
// as a side effect. If a call fails (returns false), unification
// fails.
//
// In the unification context, structural identity ignores the
// difference between a defined type and its underlying type.
// It also ignores the difference between an (external, unbound)
// type parameter and its core type.
// If two types are not structurally identical, they cannot be Go
// identical types. On the other hand, if they are structurally
// identical, they may be Go identical or at least assignable, or
// they may be in the type set of a constraint.
// Whether they indeed are identical or assignable is determined
// upon instantiation and function argument passing.

package types2

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
)

const (
	// Upper limit for recursion depth. Used to catch infinite recursions
	// due to implementation issues (e.g., see issues go.dev/issue/48619, go.dev/issue/48656).
	unificationDepthLimit = 50

	// Whether to panic when unificationDepthLimit is reached.
	// If disabled, a recursion depth overflow results in a (quiet)
	// unification failure.
	panicAtUnificationDepthLimit = true

	// If enableCoreTypeUnification is set, unification will consider
	// the core types, if any, of non-local (unbound) type parameters.
	enableCoreTypeUnification = true

	// If traceInference is set, unification will print a trace of its operation.
	// Interpretation of trace:
	//   x ≡ y    attempt to unify types x and y
	//   p ➞ y    type parameter p is set to type y (p is inferred to be y)
	//   p ⇄ q    type parameters p and q match (p is inferred to be q and vice versa)
	//   x ≢ y    types x and y cannot be unified
	//   [p, q, ...] ➞ [x, y, ...]    mapping from type parameters to types
	traceInference = false
)

// A unifier maintains a list of type parameters and
// corresponding types inferred for each type parameter.
// A unifier is created by calling newUnifier.
type unifier struct {
	// handles maps each type parameter to its inferred type through
	// an indirection *Type called (inferred type) "handle".
	// Initially, each type parameter has its own, separate handle,
	// with a nil (i.e., not yet inferred) type.
	// After a type parameter P is unified with a type parameter Q,
	// P and Q share the same handle (and thus type). This ensures
	// that inferring the type for a given type parameter P will
	// automatically infer the same type for all other parameters
	// unified (joined) with P.
	handles map[*TypeParam]*Type
	depth   int // recursion depth during unification
}

// newUnifier returns a new unifier initialized with the given type parameter
// and corresponding type argument lists. The type argument list may be shorter
// than the type parameter list, and it may contain nil types. Matching type
// parameters and arguments must have the same index.
func newUnifier(tparams []*TypeParam, targs []Type) *unifier {
	assert(len(tparams) >= len(targs))
	handles := make(map[*TypeParam]*Type, len(tparams))
	// Allocate all handles up-front: in a correct program, all type parameters
	// must be resolved and thus eventually will get a handle.
	// Also, sharing of handles caused by unified type parameters is rare and
	// so it's ok to not optimize for that case (and delay handle allocation).
	for i, x := range tparams {
		var t Type
		if i < len(targs) {
			t = targs[i]
		}
		handles[x] = &t
	}
	return &unifier{handles, 0}
}

// unify attempts to unify x and y and reports whether it succeeded.
// As a side-effect, types may be inferred for type parameters.
func (u *unifier) unify(x, y Type) bool {
	return u.nify(x, y, nil)
}

func (u *unifier) tracef(format string, args ...interface{}) {
	fmt.Println(strings.Repeat(".  ", u.depth) + sprintf(nil, true, format, args...))
}

// String returns a string representation of the current mapping
// from type parameters to types.
func (u *unifier) String() string {
	// sort type parameters for reproducible strings
	tparams := make(typeParamsById, len(u.handles))
	i := 0
	for tpar := range u.handles {
		tparams[i] = tpar
		i++
	}
	sort.Sort(tparams)

	var buf bytes.Buffer
	w := newTypeWriter(&buf, nil)
	w.byte('[')
	for i, x := range tparams {
		if i > 0 {
			w.string(", ")
		}
		w.typ(x)
		w.string(": ")
		w.typ(u.at(x))
	}
	w.byte(']')
	return buf.String()
}

type typeParamsById []*TypeParam

func (s typeParamsById) Len() int           { return len(s) }
func (s typeParamsById) Less(i, j int) bool { return s[i].id < s[j].id }
func (s typeParamsById) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

// join unifies the given type parameters x and y.
// If both type parameters already have a type associated with them
// and they are not joined, join fails and returns false.
func (u *unifier) join(x, y *TypeParam) bool {
	if traceInference {
		u.tracef("%s ⇄ %s", x, y)
	}
	switch hx, hy := u.handles[x], u.handles[y]; {
	case hx == hy:
		// Both type parameters already share the same handle. Nothing to do.
	case *hx != nil && *hy != nil:
		// Both type parameters have (possibly different) inferred types. Cannot join.
		return false
	case *hx != nil:
		// Only type parameter x has an inferred type. Use handle of x.
		u.setHandle(y, hx)
	// This case is treated like the default case.
	// case *hy != nil:
	// 	// Only type parameter y has an inferred type. Use handle of y.
	//	u.setHandle(x, hy)
	default:
		// Neither type parameter has an inferred type. Use handle of y.
		u.setHandle(x, hy)
	}
	return true
}

// asTypeParam returns x.(*TypeParam) if x is a type parameter recorded with u.
// Otherwise, the result is nil.
func (u *unifier) asTypeParam(x Type) *TypeParam {
	if x, _ := x.(*TypeParam); x != nil {
		if _, found := u.handles[x]; found {
			return x
		}
	}
	return nil
}

// setHandle sets the handle for type parameter x
// (and all its joined type parameters) to h.
func (u *unifier) setHandle(x *TypeParam, h *Type) {
	hx := u.handles[x]
	assert(hx != nil)
	for y, hy := range u.handles {
		if hy == hx {
			u.handles[y] = h
		}
	}
}

// at returns the (possibly nil) type for type parameter x.
func (u *unifier) at(x *TypeParam) Type {
	return *u.handles[x]
}

// set sets the type t for type parameter x;
// t must not be nil.
func (u *unifier) set(x *TypeParam, t Type) {
	assert(t != nil)
	if traceInference {
		u.tracef("%s ➞ %s", x, t)
	}
	*u.handles[x] = t
}

// unknowns returns the number of type parameters for which no type has been set yet.
func (u *unifier) unknowns() int {
	n := 0
	for _, h := range u.handles {
		if *h == nil {
			n++
		}
	}
	return n
}

// inferred returns the list of inferred types for the given type parameter list.
// The result is never nil and has the same length as tparams; result types that
// could not be inferred are nil. Corresponding type parameters and result types
// have identical indices.
func (u *unifier) inferred(tparams []*TypeParam) []Type {
	list := make([]Type, len(tparams))
	for i, x := range tparams {
		list[i] = u.at(x)
	}
	return list
}

// nify implements the core unification algorithm which is an
// adapted version of Checker.identical. For changes to that
// code the corresponding changes should be made here.
// Must not be called directly from outside the unifier.
func (u *unifier) nify(x, y Type, p *ifacePair) (result bool) {
	u.depth++
	if traceInference {
		u.tracef("%s ≡ %s", x, y)
	}
	defer func() {
		if traceInference && !result {
			u.tracef("%s ≢ %s", x, y)
		}
		u.depth--
	}()

	// nothing to do if x == y
	if x == y {
		return true
	}

	// Stop gap for cases where unification fails.
	if u.depth > unificationDepthLimit {
		if traceInference {
			u.tracef("depth %d >= %d", u.depth, unificationDepthLimit)
		}
		if panicAtUnificationDepthLimit {
			panic("unification reached recursion depth limit")
		}
		return false
	}

	// Unification is symmetric, so we can swap the operands.
	// Ensure that if we have at least one
	// - defined type, make sure one is in y
	// - type parameter recorded with u, make sure one is in x
	if _, ok := x.(*Named); ok || u.asTypeParam(y) != nil {
		if traceInference {
			u.tracef("%s ≡ %s (swap)", y, x)
		}
		x, y = y, x
	}

	// Unification will fail if we match a defined type against a type literal.
	// Per the (spec) assignment rules, assignments of values to variables with
	// the same type structure are permitted as long as at least one of them
	// is not a defined type. To accomodate for that possibility, we continue
	// unification with the underlying type of a defined type if the other type
	// is a type literal.
	// We also continue if the other type is a basic type because basic types
	// are valid underlying types and may appear as core types of type constraints.
	// If we exclude them, inferred defined types for type parameters may not
	// match against the core types of their constraints (even though they might
	// correctly match against some of the types in the constraint's type set).
	// Finally, if unification (incorrectly) succeeds by matching the underlying
	// type of a defined type against a basic type (because we include basic types
	// as type literals here), and if that leads to an incorrectly inferred type,
	// we will fail at function instantiation or argument assignment time.
	//
	// If we have at least one defined type, there is one in y.
	if ny, _ := y.(*Named); ny != nil && isTypeLit(x) {
		if traceInference {
			u.tracef("%s ≡ under %s", x, ny)
		}
		y = ny.under()
		// Per the spec, a defined type cannot have an underlying type
		// that is a type parameter.
		assert(!isTypeParam(y))
		// x and y may be identical now
		if x == y {
			return true
		}
	}

	// Cases where at least one of x or y is a type parameter recorded with u.
	// If we have at least one type parameter, there is one in x.
	// If we have exactly one type parameter, because it is in x,
	// isTypeLit(x) is false and y was not changed above. In other
	// words, if y was a defined type, it is still a defined type
	// (relevant for the logic below).
	switch px, py := u.asTypeParam(x), u.asTypeParam(y); {
	case px != nil && py != nil:
		// both x and y are type parameters
		if u.join(px, py) {
			return true
		}
		// both x and y have an inferred type - they must match
		return u.nify(u.at(px), u.at(py), p)

	case px != nil:
		// x is a type parameter, y is not
		if x := u.at(px); x != nil {
			// x has an inferred type which must match y
			if u.nify(x, y, p) {
				// If we have a match, possibly through underlying types,
				// and y is a defined type, make sure we record that type
				// for type parameter x, which may have until now only
				// recorded an underlying type (go.dev/issue/43056).
				if _, ok := y.(*Named); ok {
					u.set(px, y)
				}
				return true
			}
			return false
		}
		// otherwise, infer type from y
		u.set(px, y)
		return true
	}

	// If we get here and x or y is a type parameter, they are unbound
	// (not recorded with the unifier).
	// By definition, a valid type argument must be in the type set of
	// the respective type constraint. Therefore, the type argument's
	// underlying type must be in the set of underlying types of that
	// constraint. If there is a single such underlying type, it's the
	// constraint's core type. It must match the type argument's under-
	// lying type, irrespective of whether the actual type argument,
	// which may be a defined type, is actually in the type set (that
	// will be determined at instantiation time).
	// Thus, if we have the core type of an unbound type parameter,
	// we know the structure of the possible types satisfying such
	// parameters. Use that core type for further unification
	// (see go.dev/issue/50755 for a test case).
	if enableCoreTypeUnification {
		// swap x and y as needed
		// (the earlier swap checks for _recorded_ type parameters only)
		if isTypeParam(y) {
			if traceInference {
				u.tracef("%s ≡ %s (swap)", y, x)
			}
			x, y = y, x
		}
		if isTypeParam(x) {
			// When considering the type parameter for unification
			// we look at the core type.
			// Because the core type is always an underlying type,
			// unification will take care of matching against a
			// defined or literal type automatically.
			// If y is also an unbound type parameter, we will end
			// up here again with x and y swapped, so we don't
			// need to take care of that case separately.
			if cx := coreType(x); cx != nil {
				if traceInference {
					u.tracef("core %s ≡ %s", x, y)
				}
				return u.nify(cx, y, p)
			}
		}
	}

	// x != y if we reach here
	assert(x != y)

	switch x := x.(type) {
	case *Basic:
		// Basic types are singletons except for the rune and byte
		// aliases, thus we cannot solely rely on the x == y check
		// above. See also comment in TypeName.IsAlias.
		if y, ok := y.(*Basic); ok {
			return x.kind == y.kind
		}

	case *Array:
		// Two array types are identical if they have identical element types
		// and the same array length.
		if y, ok := y.(*Array); ok {
			// If one or both array lengths are unknown (< 0) due to some error,
			// assume they are the same to avoid spurious follow-on errors.
			return (x.len < 0 || y.len < 0 || x.len == y.len) && u.nify(x.elem, y.elem, p)
		}

	case *Slice:
		// Two slice types are identical if they have identical element types.
		if y, ok := y.(*Slice); ok {
			return u.nify(x.elem, y.elem, p)
		}

	case *Struct:
		// Two struct types are identical if they have the same sequence of fields,
		// and if corresponding fields have the same names, and identical types,
		// and identical tags. Two embedded fields are considered to have the same
		// name. Lower-case field names from different packages are always different.
		if y, ok := y.(*Struct); ok {
			if x.NumFields() == y.NumFields() {
				for i, f := range x.fields {
					g := y.fields[i]
					if f.embedded != g.embedded ||
						x.Tag(i) != y.Tag(i) ||
						!f.sameId(g.pkg, g.name) ||
						!u.nify(f.typ, g.typ, p) {
						return false
					}
				}
				return true
			}
		}

	case *Pointer:
		// Two pointer types are identical if they have identical base types.
		if y, ok := y.(*Pointer); ok {
			return u.nify(x.base, y.base, p)
		}

	case *Tuple:
		// Two tuples types are identical if they have the same number of elements
		// and corresponding elements have identical types.
		if y, ok := y.(*Tuple); ok {
			if x.Len() == y.Len() {
				if x != nil {
					for i, v := range x.vars {
						w := y.vars[i]
						if !u.nify(v.typ, w.typ, p) {
							return false
						}
					}
				}
				return true
			}
		}

	case *Signature:
		// Two function types are identical if they have the same number of parameters
		// and result values, corresponding parameter and result types are identical,
		// and either both functions are variadic or neither is. Parameter and result
		// names are not required to match.
		// TODO(gri) handle type parameters or document why we can ignore them.
		if y, ok := y.(*Signature); ok {
			return x.variadic == y.variadic &&
				u.nify(x.params, y.params, p) &&
				u.nify(x.results, y.results, p)
		}

	case *Interface:
		// Two interface types are identical if they have the same set of methods with
		// the same names and identical function types. Lower-case method names from
		// different packages are always different. The order of the methods is irrelevant.
		if y, ok := y.(*Interface); ok {
			xset := x.typeSet()
			yset := y.typeSet()
			if xset.comparable != yset.comparable {
				return false
			}
			if !xset.terms.equal(yset.terms) {
				return false
			}
			a := xset.methods
			b := yset.methods
			if len(a) == len(b) {
				// Interface types are the only types where cycles can occur
				// that are not "terminated" via named types; and such cycles
				// can only be created via method parameter types that are
				// anonymous interfaces (directly or indirectly) embedding
				// the current interface. Example:
				//
				//    type T interface {
				//        m() interface{T}
				//    }
				//
				// If two such (differently named) interfaces are compared,
				// endless recursion occurs if the cycle is not detected.
				//
				// If x and y were compared before, they must be equal
				// (if they were not, the recursion would have stopped);
				// search the ifacePair stack for the same pair.
				//
				// This is a quadratic algorithm, but in practice these stacks
				// are extremely short (bounded by the nesting depth of interface
				// type declarations that recur via parameter types, an extremely
				// rare occurrence). An alternative implementation might use a
				// "visited" map, but that is probably less efficient overall.
				q := &ifacePair{x, y, p}
				for p != nil {
					if p.identical(q) {
						return true // same pair was compared before
					}
					p = p.prev
				}
				if debug {
					assertSortedMethods(a)
					assertSortedMethods(b)
				}
				for i, f := range a {
					g := b[i]
					if f.Id() != g.Id() || !u.nify(f.typ, g.typ, q) {
						return false
					}
				}
				return true
			}
		}

	case *Map:
		// Two map types are identical if they have identical key and value types.
		if y, ok := y.(*Map); ok {
			return u.nify(x.key, y.key, p) && u.nify(x.elem, y.elem, p)
		}

	case *Chan:
		// Two channel types are identical if they have identical value types.
		if y, ok := y.(*Chan); ok {
			return u.nify(x.elem, y.elem, p)
		}

	case *Named:
		// TODO(gri) This code differs now from the parallel code in Checker.identical. Investigate.
		if y, ok := y.(*Named); ok {
			xargs := x.TypeArgs().list()
			yargs := y.TypeArgs().list()

			if len(xargs) != len(yargs) {
				return false
			}

			// TODO(gri) This is not always correct: two types may have the same names
			//           in the same package if one of them is nested in a function.
			//           Extremely unlikely but we need an always correct solution.
			if x.obj.pkg == y.obj.pkg && x.obj.name == y.obj.name {
				for i, x := range xargs {
					if !u.nify(x, yargs[i], p) {
						return false
					}
				}
				return true
			}
		}

	case *TypeParam:
		// nothing to do - we know x != y

	case nil:
		// avoid a crash in case of nil type

	default:
		panic(sprintf(nil, true, "u.nify(%s, %s)", x, y))
	}

	return false
}
