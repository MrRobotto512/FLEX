package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	webview2 "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
)

// The player: `FLEX.exe --player [url]` is a second, plain process of the same
// exe (no server, no tray) showing the web client full screen through the
// WebView2 runtime, so a PC under the TV plays what the server (this one, or
// any linked PC) decides, decoded by the GPU: 4K when the file is, with the
// Edge engine's HEVC when the extension is installed. The page knows it is
// the player (window.FLEX_CLIENT), so a controller works like the keys, the
// pointer hides, and Q leaves. Alt+F4 always works.

// runPlayer shows the player window and returns when it is closed.
func runPlayer(url string) {
	if url == "" {
		url = fmt.Sprintf("http://127.0.0.1:%d/", loadSettings().HTTPPort)
	}
	dataPath := filepath.Join(os.Getenv("APPDATA"), "FLEX", "player")
	_ = os.MkdirAll(dataPath, 0o755)

	user32 := windows.NewLazySystemDLL("user32.dll")
	// sharp text on a scaled display: per-monitor DPI awareness, set before any window exists
	const perMonitorAwareV2 = ^uintptr(3) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 (-4)
	user32.NewProc("SetProcessDpiAwarenessContext").Call(perMonitorAwareV2)
	getMetric := user32.NewProc("GetSystemMetrics")
	sw, _, _ := getMetric.Call(0) // SM_CXSCREEN
	sh, _, _ := getMetric.Call(1) // SM_CYSCREEN

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		DataPath:  dataPath,
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title:  "FLEX",
			Width:  uint(sw),
			Height: uint(sh),
			Center: true,
		},
	})
	if w == nil {
		fmt.Fprintln(os.Stderr, "FLEX player: the WebView2 runtime is missing (it comes with Windows 11 and Edge)")
		return
	}
	defer w.Destroy()
	w.SetTitle("FLEX")
	w.Init("window.FLEX_CLIENT = 'player';")
	w.Bind("flexQuit", func() { w.Terminate() })

	// a borderless window the size of the screen: no title bar, no taskbar
	hwnd := uintptr(w.Window())
	const gwlStyle = ^uintptr(15) // GWL_STYLE (-16)
	const wsPopup, wsVisible = 0x80000000, 0x10000000
	user32.NewProc("SetWindowLongPtrW").Call(hwnd, gwlStyle, wsPopup|wsVisible)
	const swpFrameChanged, swpShowWindow = 0x0020, 0x0040
	user32.NewProc("SetWindowPos").Call(hwnd, 0, 0, 0, sw, sh, swpFrameChanged|swpShowWindow)

	w.Navigate(url)
	w.Run()
}

// OpenPlayer starts the full-screen player on this PC's own library (which
// lists the linked PCs' too).
func (a *App) OpenPlayer() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "--player", fmt.Sprintf("http://127.0.0.1:%d/", a.settings.HTTPPort))
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}
