package main

import (
	"embed"
	"os"
	"path/filepath"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	app := NewApp()

	err := wails.Run(&options.App{
		Title:     "Photo BG",
		Width:     1220,
		Height:    780,
		MinWidth:  980,
		MinHeight: 660,

		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 245, G: 245, B: 247, A: 1},

		// Native Wails v2 file drop: Go receives full absolute paths via OnFileDrop
		// (registered in app.OnDomReady) and the WebView's own drop handling is off.
		DragAndDrop: &options.DragAndDrop{
			EnableFileDrop:     true,
		},

		OnStartup:  app.OnStartup,
		OnDomReady: app.OnDomReady,

		Bind: []interface{}{
			app,
		},

		Windows: &windows.Options{
			WebviewUserDataPath: webviewUserDataPath(),
		},
	})
	if err != nil {
		println("Error:", err.Error())
	}
}

// webviewUserDataPath keeps WebView2 profile data in a private per-app folder.
func webviewUserDataPath() string {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
		return filepath.Join(base, "PhotoBG", "WebView2")
}
