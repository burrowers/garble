// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"fmt"
	"os"
	"strings"
)

// tinyPanicRuntimeSource runs before type checking so the injected runtime
// declarations and references participate in ordinary identifier obfuscation.
func tinyPanicRuntimeSource(path, basename string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	src := string(data)
	replace := func(old, new string) error {
		if strings.Count(src, old) != 1 {
			return fmt.Errorf("tiny panic runtime patch did not match %s: %q", basename, old)
		}
		src = strings.Replace(src, old, new, 1)
		return nil
	}
	if basename == "runtime2.go" {
		err = replace("type _panic struct {", "type _panic struct {\n\tgarbleOrigin string")
		return src, err
	}
	if err = replace("p.arg = e", "p.arg = e\n\tp.garbleOrigin = garblePanicOrigin()"); err != nil {
		return "", err
	}
	start := strings.Index(src, "func printpanics(p *_panic) {")
	if start < 0 {
		return "", fmt.Errorf("tiny panic runtime patch did not match printpanics")
	}
	endOffset := strings.Index(src[start:], "\n}")
	if endOffset < 0 {
		return "", fmt.Errorf("tiny panic runtime patch did not match printpanics body")
	}
	end := start + endOffset + len("\n}")
	src = src[:start] + `func printpanics(p *_panic) {
	if p.garbleOrigin == "" {
		println("panic: hidden")
	} else {
		print("panic: p_", p.garbleOrigin, ".go:1\n")
	}
}` + src[end:]
	src += `
// garblePanicOrigin captures only the originating site, before deferred calls
// can recover or replace the panic. It never formats the panic value.
func garblePanicOrigin() string {
	gp := getg()
	var origin string
	systemstack(func() {
		var u unwinder
		for u.init(gp, unwindSilentErrors); u.valid(); u.next() {
			f := u.frame.fn
			fileno, _ := pcvalue(f, f.pcfile, u.symPC(), false)
			if fileno < 0 {
				continue
			}
			file := funcfile(f, fileno)
			if file != "?" && file != "" {
				origin = file
				return
			}
		}
	})
	return origin
}
`
	return src, nil
}
