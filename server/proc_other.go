//go:build !windows

package server

import "os/exec"

// noWindow only matters on Windows; see proc_windows.go.
func noWindow(cmd *exec.Cmd) *exec.Cmd { return cmd }
