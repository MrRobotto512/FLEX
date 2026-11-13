//go:build windows && !setup

package main

// Built without -tags setup there is no FLEX.exe inside: the program only
// says so (this keeps `go vet ./...` working without the 50 MB file).
var payload []byte
