package main

import (
"embed"
"os"
"path/filepath"
"runtime"
"sort"
"strings"
"syscall"
"unsafe"

"github.com/wailsapp/wails/v2"
"github.com/wailsapp/wails/v2/pkg/options"
"github.com/wailsapp/wails/v2/pkg/options/assetserver"
"github.com/wailsapp/wails/v2/pkg/options/windows"
)

//go:embed all:frontend/dist
var assets embed.FS

func showErrorBox(title, msg string) {
if runtime.GOOS == "windows" {
user32 := syscall.NewLazyDLL("user32.dll")
msgBox := user32.NewProc("MessageBoxW")
tPtr, _ := syscall.UTF16PtrFromString(title)
mPtr, _ := syscall.UTF16PtrFromString(msg)
msgBox.Call(0, uintptr(unsafe.Pointer(mPtr)), uintptr(unsafe.Pointer(tPtr)), 0x00000010)
}
}

// विन्डोजमा भएको वास्तविक WebView2 वा Edge इन्जिनको पाथ सिधै पत्ता लगाउने फंक्सन (पपअप रोक्न)
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

wvBrowserPath := findSystemWebView2()
_ = strings.TrimSpace(wvBrowserPath)

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
WebviewBrowserPath:  wvBrowserPath,
WebviewUserDataPath: userData,
Theme:               windows.SystemDefault,
},
OnStartup: app.startup,
Bind: []interface{}{
app,
},
})

if err != nil {
showErrorBox("Photo BG Error", "Application failed to start:\n\n"+err.Error())
}
}