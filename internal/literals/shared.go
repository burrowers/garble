// Copyright (c) 2020, The Garble Authors.
// See LICENSE for licensing information.

package literals

import (
	"go/ast"
	mathrand "math/rand"
	"strconv"

	ah "github.com/burrowers/garble/internal/asthelper"
)

// Share one randomly selected decoder within an owner: an original function or
// method (including nested closures), a global factory, or the file's remaining
// package initializers. Sharing across owners would retain dead decoders.
const maxSizeShared = 24

type sharedDecoder struct {
	obf  obfuscator
	seed int64
	call ast.Expr
}

// Only code-shaping choices are replayed for an owner. Ciphertext, keys, record
// streams, junk and bounds are generated afresh and passed as decoder inputs.
type decoderInputs struct {
	rand   *mathrand.Rand
	params *ast.FieldList
	args   []ast.Expr
}

func (n *generatedNames) input(value, typ ast.Expr) ast.Expr {
	if n.inputs == nil {
		return value
	}
	name := n.ident("input" + strconv.Itoa(len(n.inputs.args)))
	n.inputs.params.List = append(n.inputs.params.List, ah.Field(typ, name))
	n.inputs.args = append(n.inputs.args, value)
	return ast.NewIdent(name.Name)
}

func (n *generatedNames) inputInt(value int) ast.Expr {
	return n.input(ah.IntLit(value), ast.NewIdent("int"))
}

func (n *generatedNames) codeRand(rand *mathrand.Rand) *mathrand.Rand {
	if n.inputs != nil {
		return n.inputs.rand
	}
	return rand
}

func (n *generatedNames) byteLit(rand *mathrand.Rand, value byte, keys []*externalKey, prob externalKeyProbability) ast.Expr {
	if n.inputs != nil {
		return n.input(ah.IntLit(int(value)), ast.NewIdent("byte"))
	}
	return byteLitWithExtKey(rand, value, keys, prob)
}

func (r *obfRand) obfuscateSharedString(data string) *ast.CallExpr {
	d := r.proxyDispatcher
	if d.sharedDecoder == nil {
		d.sharedDecoder = &sharedDecoder{obf: Obfuscators[r.rnd.Intn(len(Obfuscators))], seed: r.rnd.Int63()}
	}
	shared := d.sharedDecoder
	junk := make([]byte, r.rnd.Intn(maxStringJunkBytes-minStringJunkBytes)+minStringJunkBytes)
	r.rnd.Read(junk)
	low := r.rnd.Intn(len(junk))
	payload := append(append(junk[:low:low], []byte(data)...), junk[low:]...)
	names := newGeneratedNames(r)
	names.inputs = &decoderInputs{rand: mathrand.New(mathrand.NewSource(shared.seed)), params: &ast.FieldList{}}
	body := shared.obf.obfuscate(r.rnd, names, payload, nil)
	lowExpr, highExpr := names.inputInt(low), names.inputInt(low+len(data))
	cast, castType := r.stringConversionAdapter()
	castExpr := names.input(cast, cloneGeneratedType(castType))
	body.List = append(body.List, ah.ReturnStmt(ah.CallExpr(castExpr, names.ident("data"), lowExpr, highExpr)))
	if shared.call == nil {
		shared.call = r.liftCall(names.inputs.params, ast.NewIdent("string"), body, nil).Fun
	}
	call := ah.CallExpr(d.cloneStringCall(shared.call), names.inputs.args...)
	if r.stringCalls != nil {
		r.stringCalls[data] = call
		return d.cloneStringCall(call).(*ast.CallExpr)
	}
	return call
}
