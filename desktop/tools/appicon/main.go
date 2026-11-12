// Command appicon turns desktop/appicon.ico (drawn by tools/logo) into the
// Windows resource objects that give FLEX.exe and flexd.exe their icon in
// Explorer, the taskbar and the window's title bar. `go build` links a
// rsrc_windows_amd64.syso in the package folder by itself.
//
//	cd desktop && go run ./tools/appicon
package main

import (
	"fmt"
	"os"

	"github.com/tc-hib/winres"
)

// Wails loads the window icon from icon resource 3 (winc.AppIconID).
const appIconID = 3

func main() {
	f, err := os.Open("appicon.ico")
	if err != nil {
		panic(err)
	}
	defer f.Close()
	icon, err := winres.LoadICO(f)
	if err != nil {
		panic(err)
	}
	rs := winres.ResourceSet{}
	if err := rs.SetIcon(winres.ID(appIconID), icon); err != nil {
		panic(err)
	}
	for _, out := range []string{"rsrc_windows_amd64.syso", "../cmd/flexd/rsrc_windows_amd64.syso"} {
		w, err := os.Create(out)
		if err != nil {
			panic(err)
		}
		if err := rs.WriteObject(w, winres.ArchAMD64); err != nil {
			panic(err)
		}
		w.Close()
		st, _ := os.Stat(out)
		fmt.Printf("%-40s %d bytes\n", out, st.Size())
	}
}
