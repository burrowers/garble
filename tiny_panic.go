// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"fmt"
	"go/ast"
	"go/constant"
	"os"
	"strings"
)

// tinyPanicKey returns the key used to hash panic messages. It derives from the
// same seed as the obfuscated names, so a printed token cannot be matched
// against a message without knowing the build's seed.
func tinyPanicKey() uint64 {
	hi := runtimeHashWithCustomSalt([]byte("tiny panic high"))
	lo := runtimeHashWithCustomSalt([]byte("tiny panic low"))
	return uint64(hi)<<32 | uint64(lo)
}

// tinyPanicToken returns the token printed for a string panic message.
func tinyPanicToken(msg string) string {
	h := tinyPanicKey()
	for i := 0; i < len(msg); i++ {
		h ^= uint64(msg[i])
		h *= 1099511628211
	}
	return fmt.Sprintf("%016x", h)
}

// tinyPanicRuntimeSource runs before type checking so the injected runtime
// declarations and references participate in ordinary identifier obfuscation.
func tinyPanicRuntimeSource(path, basename string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	src := string(data)
	switch basename {
	case "panic.go":
		start := strings.Index(src, "func printpanics(p *_panic) {")
		if start < 0 {
			return "", fmt.Errorf("tiny panic runtime patch did not match printpanics")
		}
		endOffset := strings.Index(src[start:], "\n}\n")
		if endOffset < 0 {
			return "", fmt.Errorf("tiny panic runtime patch did not match printpanics body")
		}
		end := start + endOffset + len("\n}\n")
		src = src[:start] + `func printpanics(p *_panic) {
	garblePrintPanic(p.arg)
}
` + src[end:]
		return src, nil
	case "print.go":
		// print.go is the one runtime file whose print calls are kept, so the
		// panic diagnostic lives here rather than next to printpanics.
		src += fmt.Sprintf(`
// garblePrintPanic prints one diagnostic line for an unrecovered panic. It
// never formats the value, so user-defined formatting methods do not run.
func garblePrintPanic(v any) {
	msg, ok := v.(string)
	if !ok {
		println("panic: hidden")
		return
	}
	h := uint64(%d)
	for i := 0; i < len(msg); i++ {
		h ^= uint64(msg[i])
		h *= 1099511628211
	}
	digits := "0123456789abcdef"
	print("panic: ")
	for shift := 60; shift >= 0; shift -= 4 {
		d := byte(h>>uint(shift)) & 0xf
		print(digits[d : d+1])
	}
	print("\n")
}
`, tinyPanicKey())
		return src, nil
	}
	return src, nil
}

// tinyPanicMessages finds the string message of each panic call in a package.
func tinyPanicMessages(tf *transformer, file *ast.File) map[string]string {
	msgs := make(map[string]string)
	for node := range ast.Preorder(file) {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			continue
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || id.Name != "panic" {
			continue
		}
		tv, ok := tf.info.Types[call.Args[0]]
		if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
			continue
		}
		msg := constant.StringVal(tv.Value)
		msgs[tinyPanicToken(msg)] = msg
	}
	return msgs
}
