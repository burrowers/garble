module mvdan.cc/garble

// Go 1.26.2 fixed a fairly common Windows runtime crash.
go 1.26.2

require (
	github.com/bluekeyes/go-gitdiff v0.9.0
	github.com/go-quicktest/qt v1.102.0
	github.com/google/go-cmp v0.7.0
	github.com/pkg/browser v0.0.0-20240102092130-5ac0b6a4141c
	github.com/rogpeppe/go-internal v1.15.0
	github.com/tinylib/msgp v1.6.4
	golang.org/x/mod v0.38.0
	golang.org/x/tools v0.48.0
)

require (
	github.com/kr/pretty v0.3.1 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/philhofer/fwd v1.2.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

tool (
	github.com/tinylib/msgp
	golang.org/x/tools/cmd/bundle
)
