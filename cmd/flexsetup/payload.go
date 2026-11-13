//go:build windows && setup

package main

import _ "embed"

// FLEX.exe is copied next to this file by the release build before
// `go build -tags setup`.
//
//go:embed FLEX.exe
var payload []byte
