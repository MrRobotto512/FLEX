package server

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// detectGPUs names the graphics adapters, to explain the encoder choice.
// Windows only (WMI through PowerShell); elsewhere it returns nothing.
func detectGPUs() []string {
	if runtime.GOOS != "windows" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := noWindow(exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command",
		"(Get-CimInstance Win32_VideoController | ForEach-Object { $_.Name + ' (driver ' + $_.DriverVersion + ')' }) -join \"`n\"")).Output()
	if err != nil {
		return nil
	}
	var gpus []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			gpus = append(gpus, l)
		}
	}
	return gpus
}
