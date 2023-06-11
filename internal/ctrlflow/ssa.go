package ctrlflow

import (
	"go/constant"
	"go/types"
	"reflect"
	"unsafe"

	"golang.org/x/tools/go/ssa"
)

func setUnexportableField(objRaw interface{}, name string, valRaw interface{}) {
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
	setUnexportableField(block, "parent", ssaFunc)
}

func setBlock(instr ssa.Instruction, block *ssa.BasicBlock) {
	setUnexportableField(instr, "block", block)
}

func setType(instr ssa.Instruction, typ types.Type) {
	setUnexportableField(instr, "typ", typ)
}

func makeSsaInt(i int) *ssa.Const {
	return ssa.NewConst(constant.MakeInt64(int64(i)), types.Typ[types.Int])
}
