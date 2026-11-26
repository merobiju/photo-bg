package main

import (
	"embed"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

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
			DisableWebViewDrop: true,
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

// findSystemWebView2 returns the newest system-installed WebView2 runtime folder
// (the one that contains msedgewebview2.exe), or "" to let Wails use its default.
// It is a no-op on macOS and Linux.
func findSystemWebView2() string {
	if runtime.GOOS != "windows" {
		return ""
	}

	var roots []string
	for _, key := range []string{"ProgramFiles(x86)", "ProgramFiles"} {
		if v := os.Getenv(key); v != "" {
			roots = append(roots, v)
		}
	}

	best := ""
	var bestVer []int
	for _, root := range roots {
		base := filepath.Join(root, "Microsoft", "EdgeWebView", "Application")
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			ver := parseVersion(e.Name())
			if ver == nil {
				continue
			}
			dir := filepath.Join(base, e.Name())
			if _, err := os.Stat(filepath.Join(dir, "msedgewebview2.exe")); err != nil {
				continue
			}
			if best == "" || versionLess(bestVer, ver) {
				best, bestVer = dir, ver
			}
		}
	}
	return best
}

func parseVersion(s string) []int {
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return nil
	}
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		out = append(out, n)
	}
	return out
}

func versionLess(a, b []int) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}
