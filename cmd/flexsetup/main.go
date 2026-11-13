//go:build windows

// FLEX-Setup.exe installs FLEX for the signed-in user, no administrator
// needed: FLEX.exe (carried inside this program) goes to
// %LOCALAPPDATA%\Programs\FLEX, with Start menu and desktop shortcuts and an
// entry in Windows' "Installed apps" that removes it again. A FLEX that is
// running (from wherever it was started before) is stopped first and its
// "Start with Windows" entry is pointed at the new place. Settings and the
// library's data (%APPDATA%\FLEX) are never touched, by install or uninstall.
//
//	FLEX-Setup.exe             ask, install, start FLEX
//	FLEX-Setup.exe /S          the same without questions
//	FLEX-Setup.exe /uninstall  remove (what "Installed apps" runs); /S for no questions
//
// Built by the release workflow: FLEX.exe is copied next to payload.go, then
// `go build -tags setup -ldflags "-H windowsgui -X main.version=..."`.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var version = "dev"

const (
	runKey       = `Software\Microsoft\Windows\CurrentVersion\Run`
	uninstallKey = `Software\Microsoft\Windows\CurrentVersion\Uninstall\FLEX`
	uninstaller  = "Uninstall.exe"
	idOK         = 1 // MessageBox: the OK button
)

func main() {
	silent, uninstall, cleanup := false, false, ""
	args := os.Args[1:]
	for i, a := range args {
		switch strings.ToLower(a) {
		case "/s", "-s", "--silent":
			silent = true
		case "/uninstall", "--uninstall":
			uninstall = true
		case "--remove-dir": // the uninstaller's copy in %TEMP%, finishing the job
			if i+1 < len(args) {
				cleanup = args[i+1]
			}
		}
	}
	var err error
	switch {
	case cleanup != "":
		removeDir(cleanup)
		return
	case uninstall:
		err = runUninstall(silent)
	default:
		err = runInstall(silent)
	}
	if err != nil {
		if !silent {
			message("FLEX Setup", err.Error(), windows.MB_OK|windows.MB_ICONERROR)
		}
		os.Exit(1)
	}
}

func installDir() (string, error) {
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		return "", errors.New("Windows didn't say where this user's programs go (LOCALAPPDATA is empty).")
	}
	return filepath.Join(local, "Programs", "FLEX"), nil
}

func runInstall(silent bool) error {
	if len(payload) == 0 {
		return errors.New("This FLEX-Setup.exe was built without FLEX inside it. Download it from the FLEX releases page.")
	}
	dir, err := installDir()
	if err != nil {
		return err
	}
	running := flexRunning()
	if !silent {
		text := fmt.Sprintf("Install FLEX %s for this Windows user?\n\nIt goes to %s, with shortcuts in the Start menu and on the desktop. No administrator rights are needed, and your FLEX settings and library stay as they are.", version, dir)
		if running {
			text += "\n\nFLEX is running now: it is closed, and whatever is playing stops, then the new one starts."
		}
		if message("FLEX Setup", text, windows.MB_OKCANCEL|windows.MB_ICONQUESTION) != idOK {
			return nil
		}
	}
	if err := stopFLEX(); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("Couldn't make %s: %v", dir, err)
	}
	exe := filepath.Join(dir, "FLEX.exe")
	if err := replaceFile(exe, payload); err != nil {
		return fmt.Errorf("Couldn't put FLEX.exe in %s: %v", dir, err)
	}
	// this program doubles as the uninstaller
	if self, err := os.ReadFile(must(os.Executable())); err == nil {
		replaceFile(filepath.Join(dir, uninstaller), self)
	}
	var problems []string
	for _, lnk := range shortcuts() {
		if err := makeShortcut(lnk, exe, dir); err != nil {
			problems = append(problems, "a shortcut ("+filepath.Base(filepath.Dir(lnk))+")")
		}
	}
	if err := register(dir, exe); err != nil {
		problems = append(problems, `the "Installed apps" entry`)
	}
	moved := moveAutostart(exe)

	cmd := exec.Command(exe)
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("FLEX is installed in %s but didn't start: %v", dir, err)
	}
	if !silent {
		text := fmt.Sprintf("FLEX %s is installed and starting.\n\nOpen it from the Start menu or the desktop. It keeps itself up to date from its own window.", version)
		if moved != "" {
			text += "\n\n\"Start FLEX with Windows\" now starts this copy. The old one (" + moved + ") is no longer used and can be deleted."
		}
		if len(problems) > 0 {
			text += "\n\nNot done: " + strings.Join(problems, ", ") + ". FLEX itself works."
		}
		message("FLEX Setup", text, windows.MB_OK|windows.MB_ICONINFORMATION)
	}
	return nil
}

func runUninstall(silent bool) error {
	dir, err := installDir()
	if err != nil {
		return err
	}
	if !silent && message("FLEX", "Remove FLEX from this PC?\n\nYour settings, library data and posters (%APPDATA%\\FLEX) are kept, in case you install it again.",
		windows.MB_OKCANCEL|windows.MB_ICONQUESTION) != idOK {
		return nil
	}
	if err := stopFLEX(); err != nil {
		return err
	}
	for _, lnk := range shortcuts() {
		os.Remove(lnk)
	}
	if k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE|registry.SET_VALUE); err == nil {
		if v, _, err := k.GetStringValue("FLEX"); err == nil && strings.Contains(strings.ToLower(v), strings.ToLower(dir)) {
			k.DeleteValue("FLEX")
		}
		k.Close()
	}
	registry.DeleteKey(registry.CURRENT_USER, uninstallKey)
	// A running program can't delete its own file: a copy in %TEMP% removes
	// the folder once this one has ended.
	self := must(os.Executable())
	tmp := filepath.Join(os.TempDir(), "FLEX-remove.exe")
	if b, err := os.ReadFile(self); err == nil && os.WriteFile(tmp, b, 0o755) == nil {
		exec.Command(tmp, "--remove-dir", dir).Start()
	}
	if !silent {
		message("FLEX", "FLEX is removed.", windows.MB_OK|windows.MB_ICONINFORMATION)
	}
	return nil
}

// removeDir deletes the install folder (only ever a folder named FLEX with
// FLEX's own files in it), waiting for the uninstaller in it to end.
func removeDir(dir string) {
	if filepath.Base(dir) != "FLEX" {
		return
	}
	for i := 0; i < 40; i++ {
		os.Remove(filepath.Join(dir, "FLEX.exe"))
		os.Remove(filepath.Join(dir, "FLEX.exe.old"))
		os.Remove(filepath.Join(dir, "FLEX.exe.new"))
		os.RemoveAll(filepath.Join(dir, "FLEX.exe.WebView2"))
		os.Remove(filepath.Join(dir, uninstaller))
		if os.Remove(dir) == nil || !exists(dir) {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func must(s string, _ error) string { return s }

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// replaceFile writes data to path by way of path.new; a file in use (this
// program installing over itself, say) is moved aside first.
func replaceFile(path string, data []byte) error {
	tmp := path + ".new"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err == nil {
		return nil
	}
	old := path + ".old"
	os.Remove(old)
	if err := os.Rename(path, old); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Rename(old, path)
		return err
	}
	os.Remove(old) // fails while it is still running; FLEX removes it at its next update
	return nil
}

// --- the running FLEX ---

func flexPIDs() []uint32 {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	var pids []uint32
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if strings.EqualFold(windows.UTF16ToString(e.ExeFile[:]), "FLEX.exe") && e.ProcessID != uint32(os.Getpid()) {
			pids = append(pids, e.ProcessID)
		}
	}
	return pids
}

func flexRunning() bool { return len(flexPIDs()) > 0 }

// stopFLEX ends every FLEX.exe of this user (the window, and the full-screen
// player if open) and waits until they are gone, so the file can be replaced
// and the ports are free.
func stopFLEX() error {
	for _, pid := range flexPIDs() {
		h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, pid)
		if err != nil {
			continue
		}
		windows.TerminateProcess(h, 0)
		windows.WaitForSingleObject(h, 10000)
		windows.CloseHandle(h)
	}
	for i := 0; i < 20 && flexRunning(); i++ {
		time.Sleep(250 * time.Millisecond)
	}
	if flexRunning() {
		return errors.New("FLEX is running and couldn't be closed. Quit it from its tray icon, then run this again.")
	}
	return nil
}

// --- shortcuts, registry ---

func shortcuts() []string {
	var out []string
	if appdata := os.Getenv("APPDATA"); appdata != "" {
		out = append(out, filepath.Join(appdata, "Microsoft", "Windows", "Start Menu", "Programs", "FLEX.lnk"))
	}
	if desktop, err := windows.KnownFolderPath(windows.FOLDERID_Desktop, 0); err == nil {
		out = append(out, filepath.Join(desktop, "FLEX.lnk"))
	}
	return out
}

// makeShortcut writes a .lnk through the Windows Script Host's shell object
// (PowerShell is on every Windows this runs on; the paths go in through the
// environment, so nothing in them is read as script).
func makeShortcut(lnk, target, dir string) error {
	const script = `$s = (New-Object -ComObject WScript.Shell).CreateShortcut($env:FLEX_LNK); $s.TargetPath = $env:FLEX_TARGET; $s.WorkingDirectory = $env:FLEX_DIR; $s.Description = 'FLEX media server'; $s.Save()`
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	cmd.Env = append(os.Environ(), "FLEX_LNK="+lnk, "FLEX_TARGET="+target, "FLEX_DIR="+dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	if !exists(lnk) {
		return errors.New("not written")
	}
	return nil
}

// register adds FLEX to Settings > Apps > Installed apps.
func register(dir, exe string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, uninstallKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	un := filepath.Join(dir, uninstaller)
	for name, v := range map[string]string{
		"DisplayName":          "FLEX",
		"DisplayVersion":       version,
		"Publisher":            "FLEX",
		"DisplayIcon":          exe,
		"InstallLocation":      dir,
		"UninstallString":      `"` + un + `" /uninstall`,
		"QuietUninstallString": `"` + un + `" /uninstall /S`,
		"URLInfoAbout":         "https://github.com/MrRobotto512/FLEX",
	} {
		if err := k.SetStringValue(name, v); err != nil {
			return err
		}
	}
	k.SetDWordValue("NoModify", 1)
	k.SetDWordValue("NoRepair", 1)
	k.SetDWordValue("EstimatedSize", uint32(len(payload)/1024))
	return nil
}

// moveAutostart points an existing "Start FLEX with Windows" entry at the
// installed copy and returns the folder it pointed at before, if another.
func moveAutostart(exe string) string {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, err := k.GetStringValue("FLEX")
	if err != nil || v == "" {
		return ""
	}
	old := v
	if strings.HasPrefix(old, `"`) {
		old, _, _ = strings.Cut(old[1:], `"`)
	} else {
		old, _, _ = strings.Cut(old, " ")
	}
	if strings.EqualFold(old, exe) {
		return ""
	}
	if k.SetStringValue("FLEX", `"`+exe+`" --minimized`) != nil {
		return ""
	}
	return old
}

func message(title, text string, flags uint32) int32 {
	r, _ := windows.MessageBox(0, windows.StringToUTF16Ptr(text), windows.StringToUTF16Ptr(title), flags|windows.MB_SETFOREGROUND)
	return r
}
