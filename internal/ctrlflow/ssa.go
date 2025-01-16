package ctrlflow

import (
	"go/constant"
	"go/types"
	"reflect"
	"unsafe"

	"golang.org/x/tools/go/ssa"
)

// setUnexportedField is used to modify unexported fields of ssa api structures.
// TODO: find an alternative way to access private fields or raise a feature request upstream
func setUnexportedField(objRaw any, name string, valRaw any) {
	obj := reflect.ValueOf(objRaw)
	for obj.Kind() == reflect.Pointer || obj.Kind() == reflect.Interface {
		obj = obj.Elem()
	}

	field := obj.FieldByName(name)
	if !field.IsValid() {
		panic("invalid field: " + name)
	}

	fakeStruct := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr()))
	fakeStruct.Elem().Set(reflect.ValueOf(valRaw))
}

func setBlockParent(block *ssa.BasicBlock, ssaFunc *ssa.Function) {
	setUnexportedField(block, "parent", ssaFunc)
}

func setBlock(instr ssa.Instruction, block *ssa.BasicBlock) {
	setUnexportedField(instr, "block", block)
}

func setType(instr ssa.Instruction, typ types.Type) {
	setUnexportedField(instr, "typ", typ)
}

func makeSsaInt(i int) *ssa.Const {
	return ssa.NewConst(constant.MakeInt64(int64(i)), types.Typ[types.Int])
}
