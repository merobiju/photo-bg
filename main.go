package main

import (
	"embed"
	"fmt"
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
	// Headless CLI mode: any "-flag" on the command line runs the image pipeline
	// without opening a window, e.g.
	//   Photo_BG.exe --in input.jpg --out output.png --ink blue
	// Plain file paths (Explorer "Open with", drag onto the icon) are not flags and
	// are picked up by the GUI through App.GetStartupFiles.
	if isCLIInvocation(os.Args[1:]) {
		os.Exit(runCLI(os.Args[1:]))
	}

	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintln(os.Stderr, "Fatal:", r)
			os.Exit(1)
		}
	}()

	app := NewApp()

	err := wails.Run(&options.App{
		Title:     "Photo BG",
		Width:     1280,
		Height:    820,
		MinWidth:  980,
		MinHeight: 660,

		AssetServer: &assetserver.Options{
			Assets:  assets,
			Handler: app.assetHandler(), // serves temp files for native drag-out
		},
		BackgroundColour: &options.RGBA{R: 245, G: 245, B: 247, A: 1},

		// Native Wails v2 file drop: Go receives full absolute paths via OnFileDrop
		// (registered in app.OnDomReady). NEVER set DisableWebViewDrop here: on Windows 11
		// it triggers an ICoreWebView2Controller4 nil-pointer panic (0xc0000005).
		DragAndDrop: &options.DragAndDrop{
			EnableFileDrop: true,
		},

		OnStartup:  app.OnStartup,
		OnDomReady: app.OnDomReady,
		OnShutdown: app.onShutdown,

		Bind: []interface{}{
			app,
		},

		// WebviewBrowserPath is intentionally left empty so Wails resolves the
		// WebView2 runtime through the Windows registry.
		Windows: &windows.Options{
			WebviewUserDataPath: webviewUserDataPath(),
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err.Error())
		os.Exit(1)
	}
}

// webviewUserDataPath keeps WebView2 profile data in a private, writable per-user folder.
func webviewUserDataPath() string {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "PhotoBG", "WebView2")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}
