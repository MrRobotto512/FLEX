package server

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// A child started through noWindow must have no console window, or every
// ffprobe in a scan pops one up when FLEX.exe (a GUI app) starts it. A parent
// with no console window of its own passes either way; run it from a terminal
// or as a GUI binary (go test -c -ldflags -H=windowsgui) to catch a regression.
func TestNoWindowChildHasNoConsole(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	script := `Add-Type -Name W -Namespace N -MemberDefinition '[DllImport("kernel32.dll")] public static extern System.IntPtr GetConsoleWindow();'; [N.W]::GetConsoleWindow()`
	out, err := noWindow(exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)).Output()
	if err != nil {
		t.Skipf("powershell: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "0" {
		t.Errorf("child got console window %s, want none (0)", got)
	}
}
