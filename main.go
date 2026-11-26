package main

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

//go:embed all:frontend/dist
var assets embed.FS

func findSystemWebView2() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	bases := []string{
		`C:\Program Files (x86)\Microsoft\EdgeWebView\Application`,
		`C:\Program Files\Microsoft\EdgeWebView\Application`,
		filepath.Join(os.Getenv("LOCALAPPDATA"), `Microsoft\EdgeWebView\Application`),
		`C:\Program Files (x86)\Microsoft\Edge\Application`,
		`C:\Program Files\Microsoft\Edge\Application`,
	}
	for _, base := range bases {
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		var versions []string
		for _, e := range entries {
			if e.IsDir() && len(e.Name()) > 0 && e.Name()[0] >= '0' && e.Name()[0] <= '9' {
				versions = append(versions, e.Name())
			}
		}
		if len(versions) > 0 {
			sort.Strings(versions)
			latest := filepath.Join(base, versions[len(versions)-1])
			if _, err := os.Stat(filepath.Join(latest, "msedgewebview2.exe")); err == nil {
				return latest
			}
			if _, err := os.Stat(filepath.Join(latest, "msedge.exe")); err == nil {
				return latest
			}
		}
	}
	return ""
}

func main() {
	configDir, err := os.UserConfigDir()
	if err != nil {
		configDir = os.TempDir()
	}
	userData := filepath.Join(configDir, "PhotoBG_BijayaPokhrel", "WebViewData")
	_ = os.MkdirAll(userData, 0755)

	app := NewApp()
	err = wails.Run(&options.App{
		Title:     "Photo BG — Offline HD Background & Signature Remover",
		Width:     1180,
		Height:    760,
		MinWidth:  980,
		MinHeight: 660,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		Windows: &windows.Options{
			WebviewBrowserPath:  findSystemWebView2(),
			WebviewUserDataPath: userData,
			Theme:               windows.SystemDefault,
		},
		OnStartup: app.OnStartup,
		Bind: []interface{}{
			app,
		},
	})

	if err != nil {
		fmt.Fprintln(os.Stderr, "Photo BG Error:", err)
		os.Exit(1)
	}
}
