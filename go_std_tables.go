package main

// Obtained from "go list -deps runtime" as of June 29th.
// Note that the same command on Go 1.18 results in the same list.
var runtimeAndDeps = map[string]bool{
	"internal/goarch":          true,
	"unsafe":                   true,
	"internal/abi":             true,
	"internal/cpu":             true,
	"internal/bytealg":         true,
	"internal/goexperiment":    true,
	"internal/goos":            true,
	"runtime/internal/atomic":  true,
	"runtime/internal/math":    true,
	"runtime/internal/sys":     true,
	"runtime/internal/syscall": true,
	"runtime":                  true,
}

// Obtained via scripts/runtime-linknamed-nodeps.sh as of 2022-11-01.
var runtimeLinknamed = []string{
	"arena",
	"crypto/internal/boring",
	"crypto/internal/boring/bcache",
	"crypto/internal/boring/fipstls",
	"crypto/x509/internal/macos",
	"internal/godebug",
	"internal/poll",
	"internal/reflectlite",
	"internal/syscall/unix",
	"math/rand",
	"net",
	"os",
	"os/signal",
	"plugin",
	"reflect",
	"runtime/coverage",
	"runtime/debug",
	"runtime/metrics",
	"runtime/pprof",
	"runtime/trace",
	"sync",
	"sync/atomic",
	"syscall",
	"syscall/js",
	"time",
}
