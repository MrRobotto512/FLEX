// FLEX desktop: the Windows configuration window. It embeds the server and
// keeps it running while the window is open (tray mode comes later).
package main

import (
	"embed"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	app := NewApp()
	err := wails.Run(&options.App{
		Title:      "FLEX",
		Width:      960,
		Height:     680,
		MinWidth:   720,
		MinHeight:  520,
		AssetServer: &assetserver.Options{Assets: assets},
		OnStartup:  app.startup,
		OnShutdown: app.shutdown,
		Bind:       []any{app},
	})
	if err != nil {
		panic(err)
	}
}
