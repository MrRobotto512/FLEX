package main

import (
	_ "embed"
	"fmt"
	"runtime"
	"time"

	"fyne.io/systray"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

//go:embed tray.ico
var trayIcon []byte

// startTray runs the notification-area icon. Windows delivers a window's
// messages to the thread that created it, so the tray gets its own locked
// OS thread for both creation and its message loop (Wails owns the main one).
func (a *App) startTray() {
	go func() {
		runtime.LockOSThread()
		systray.Run(a.trayReady, nil)
	}()
}

func (a *App) trayReady() {
	systray.SetIcon(trayIcon)
	systray.SetTitle("FLEX")
	systray.SetTooltip("FLEX")

	open := systray.AddMenuItem("Open FLEX", "Show the settings window")
	browse := systray.AddMenuItem("Open library in browser", "")
	scan := systray.AddMenuItem("Scan library now", "")
	systray.AddSeparator()
	status := systray.AddMenuItem("Starting…", "")
	status.Disable()
	remote := systray.AddMenuItem("Remote access: off", "")
	systray.AddSeparator()
	auto := systray.AddMenuItemCheckbox("Start with Windows", "Run FLEX in the tray when you sign in", autostartEnabled())
	update := systray.AddMenuItem("Check for updates", "Look for a newer FLEX on GitHub")
	systray.AddSeparator()
	quit := systray.AddMenuItem("Quit FLEX", "Stop the server and exit")

	go func() {
		for {
			select {
			case <-open.ClickedCh:
				a.ShowWindow()
			case <-browse.ClickedCh:
				a.OpenLibrary()
			case <-scan.ClickedCh:
				a.StartScan()
			case <-remote.ClickedCh:
				if st := a.GetStatus(); st.TSNeedsLogin {
					a.TailscaleSignIn()
				} else {
					a.ShowWindow()
				}
			case <-auto.ClickedCh:
				on := !auto.Checked()
				if err := a.SetAutostart(on); err != nil {
					a.log.Warn("autostart", "err", err)
				}
			case <-update.ClickedCh:
				if a.UpdateStatus().Available {
					update.SetTitle("Updating…")
					if err := a.InstallUpdate(); err != nil {
						a.log.Warn("update", "err", err)
						update.SetTitle("Update failed — try again")
					}
				} else {
					update.SetTitle(updateLine(a.CheckUpdate()))
				}
			case <-quit.ClickedCh:
				a.Quit()
				return
			}
		}
	}()

	// keep the status lines and the checkbox current
	go func() {
		for {
			st := a.GetStatus()
			line := "Server stopped"
			if st.Running {
				line = fmt.Sprintf("Serving %d movies · %d shows", st.Scan.Movies, st.Scan.Shows)
				if st.Scan.Scanning {
					line = fmt.Sprintf("Scanning… %d files", st.Scan.FilesSeen)
				}
				if n := len(st.Transcodes); n > 0 {
					line += fmt.Sprintf(" · converting %d for playback", n)
				}
			} else if st.Error != "" {
				line = "Error: " + st.Error
			}
			status.SetTitle(line)
			systray.SetTooltip("FLEX — " + line)
			remote.SetTitle(remoteLine(st))
			if up := a.UpdateStatus(); up.Available {
				update.SetTitle(updateLine(up))
			}
			if autostartEnabled() {
				auto.Check()
			} else {
				auto.Uncheck()
			}
			time.Sleep(5 * time.Second)
		}
	}()
}

func remoteLine(st Status) string {
	switch {
	case !st.TSEnabled:
		return "Remote access: off"
	case st.TSNeedsLogin:
		return "Remote access: sign in to Tailscale…"
	case st.TSState == "Running":
		return "Remote access: " + st.TSDNS
	default:
		return "Remote access: " + st.TSState
	}
}

func updateLine(up UpdateInfo) string {
	switch {
	case up.Available:
		return "Update to " + up.Latest + " (now " + up.Current + ")"
	case up.Error != "":
		return "Update check failed — try again"
	case up.Latest != "":
		return "Up to date (" + up.Current + ")"
	default:
		return "Check for updates"
	}
}

// ShowWindow brings the settings window back from the tray.
func (a *App) ShowWindow() {
	wruntime.WindowShow(a.ctx)
	wruntime.WindowUnminimise(a.ctx)
}

// Quit stops the server and exits (closing the window only hides it).
func (a *App) Quit() {
	systray.Quit()
	wruntime.Quit(a.ctx)
}
