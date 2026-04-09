module mvdan.cc/garble

// Go 1.26.2 fixed a fairly common Windows runtime crash.
go 1.26.2

require (
	github.com/bluekeyes/go-gitdiff v0.8.1
	github.com/go-quicktest/qt v1.101.0
	github.com/google/go-cmp v0.7.0
	github.com/rogpeppe/go-internal v1.14.1
	golang.org/x/mod v0.33.0
	golang.org/x/tools v0.42.0
)

require (
	github.com/kr/pretty v0.3.1 // indirect
	github.com/kr/text v0.2.0 // indirect
	golang.org/x/sync v0.19.0 // indirect
	golang.org/x/sys v0.41.0 // indirect
)

tool golang.org/x/tools/cmd/bundle
