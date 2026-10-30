// FLEX desktop: the Windows configuration window and tray icon. It embeds
// the server and keeps it running; closing the window hides it to the tray.
package main

import (
	"embed"
	"os"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	minimized := false // started by the "Start with Windows" entry, or after an update
	for _, a := range os.Args[1:] {
		if a == "--minimized" {
			minimized = true
		}
	}
	afterUpdate(os.Args[1:])
	app := NewApp()
	err := wails.Run(&options.App{
		Title:             "FLEX",
		Width:             960,
		Height:            760,
		MinWidth:          720,
		MinHeight:         520,
		StartHidden:       minimized,
		HideWindowOnClose: true,
		// a second FLEX.exe would fight over the ports; show this one instead
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId:               "flex-desktop-5aa9ff",
			OnSecondInstanceLaunch: func(options.SecondInstanceData) { app.ShowWindow() },
		},
		AssetServer: &assetserver.Options{Assets: assets},
		OnStartup:   app.startup,
		OnShutdown:  app.shutdown,
		Bind:        []any{app},
	})
	if err != nil {
		panic(err)
	}
}
