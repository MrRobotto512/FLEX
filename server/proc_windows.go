package server

import (
	"os/exec"
	"syscall"
)

// createNoWindow is CREATE_NO_WINDOW from winbase.h (syscall doesn't name it).
const createNoWindow = 0x08000000

// noWindow stops a console program (ffprobe, ffmpeg, powershell) from opening
// a console window of its own. FLEX.exe is a GUI app with no console to lend
// it, so without this every probe in a scan flashes up an empty window.
func noWindow(cmd *exec.Cmd) *exec.Cmd {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	return cmd
}
