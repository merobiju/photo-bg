package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"io/fs"
	"math"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	wailsrt "github.com/wailsapp/wails/v2/pkg/runtime"
	_ "golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

// ===========================================================================
// Constants
// ===========================================================================

const (
	appVersion  = "4.0.0"
	repoOwner   = "merobiju"
	repoName    = "photo-bg"
	outDirName  = "PhotoBG_Output"
	zipFileName = "PhotoBG_Export.zip"
	tempDirName = "PhotoBG"

	thumbMax       = 64   // longest side of queue thumbnails
	previewMax     = 1280 // longest side of the live-preview source
	exportCapSmall = 2400 // export source cap when no upscale is requested
	exportCapHD    = 1600 // export source cap when HD upscaling is requested
	maxOutDim      = 6000 // longest side of any final output
	maxOutPixels   = 24_000_000
	maxWorkers     = 3
	maxDropImages  = 2000
	maxBatchFiles  = 5000
	maxFolderDepth = 8

	maxDecodePixels  = 120_000_000
	maxProcessPixels = 16_000_000
	decodeBudgetPx   = 70_000_000 // concurrent native pixels allowed in a batch

	formW = 600
	formH = 200

	previewCacheMax = 6
	watchInterval   = 2 * time.Second
	maxStrokePoints = 400_000
	maxPasteBytes   = 120 << 20
)

var imageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".bmp": true, ".webp": true, ".tif": true, ".tiff": true,
}

// ===========================================================================
// Public data types (JSON shapes shared with frontend/dist/index.html)
// ===========================================================================

// CropRect is a normalized (0..1) rectangle in the rotated/flipped image frame.
type CropRect struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// Stroke is one brush stroke in normalized frame coordinates.
//
//	Tool: "erase" | "restore" | "blackout" | "pixelate"
//	Size: brush diameter as a fraction of the frame's longest side
//	Pts:  flat [x0,y0,x1,y1,...] each 0..1 of frame width / height
type Stroke struct {
	Tool string    `json:"tool"`
	Size float64   `json:"size"`
	Pts  []float64 `json:"pts"`
}

// Watermark is the anti-fraud micro-watermark. Text may contain {date}.
type Watermark struct {
	Enabled bool   `json:"enabled"`
	Text    string `json:"text"`
	Opacity int    `json:"opacity"` // 1..100
	Color   string `json:"color"`   // #RRGGBB
	Size    int    `json:"size"`    // 6..80 (relative to image size)
	Mode    string `json:"mode"`    // "diagonal" | "bottom"
}

// Settings holds every processing parameter.
type Settings struct {
	Ink          string    `json:"ink"`       // "original" or "#RRGGBB"
	Separator    string    `json:"separator"` // "all" | "signature" | "stamp"
	Transparency int       `json:"transparency"`
	Boldness     int       `json:"boldness"`
	Stroke       int       `json:"stroke"`    // -2 (erode) .. +3 (dilate)
	Despeckle    int       `json:"despeckle"` // 0 (off) .. 10
	AutoTrim     bool      `json:"autoTrim"`
	Padding      int       `json:"padding"` // 0..64
	Deskew       bool      `json:"deskew"`
	Rotate       int       `json:"rotate"` // 0, 90, 180, 270 (clockwise)
	FlipH        bool      `json:"flipH"`
	FlipV        bool      `json:"flipV"`
	Crop         *CropRect `json:"crop,omitempty"`
	Upscale      int       `json:"upscale"`    // 1..3
	FormCanvas   bool      `json:"formCanvas"` // legacy 600x200 canvas
	SizeMode     string    `json:"sizeMode"`   // "hd" | "kb500" | "mb1"
	Strokes      []Stroke  `json:"strokes,omitempty"`
	Watermark    Watermark `json:"watermark"`
}

// ExportOptions controls what is written and where.
type ExportOptions struct {
	Kind         string   `json:"kind"`    // "ink" (transparent layer) | "document" (full page)
	Formats      []string `json:"formats"` // any of png, svg, pdf, jpg
	Split        bool     `json:"split"`   // ink only: write *_signature and *_stamp separately
	OutDir       string   `json:"outDir"`
	PDFMode      string   `json:"pdfMode"`  // "a4" | "auto"
	CleanDoc     bool     `json:"cleanDoc"` // document only: normalize shadows to white paper
	OpenWhenDone bool     `json:"openWhenDone"`
	OutName      string   `json:"outName"` // exact file stem (CLI); overwrites
}

type ExportResult struct {
	OK      bool     `json:"ok"`
	Files   []string `json:"files"`
	OutDir  string   `json:"outDir"`
	Bytes   int64    `json:"bytes"`
	Message string   `json:"message"`
	Error   string   `json:"error"`
}

type PreviewResult struct {
	Clean    string  `json:"clean"`    // PNG data URL of the processed layer
	Orig     string  `json:"orig"`     // JPEG data URL of the frame (empty when unchanged)
	FrameKey string  `json:"frameKey"` // identifies Orig; resend-free when equal to the known key
	W        int     `json:"w"`
	H        int     `json:"h"`
	OrigW    int     `json:"origW"`
	OrigH    int     `json:"origH"`
	OffX     int     `json:"offX"` // top-left of Clean inside the Orig frame (may be negative)
	OffY     int     `json:"offY"`
	OutW     int     `json:"outW"` // estimated export size
	OutH     int     `json:"outH"`
	Angle    float64 `json:"angle"` // deskew rotation that was applied (degrees)
	Ms       int64   `json:"ms"`
	Empty    bool    `json:"empty"` // nothing left after processing
	Error    string  `json:"error"`
}

type StudioImage struct {
	Img     string `json:"img"` // JPEG data URL, rotated/flipped, uncropped
	W       int    `json:"w"`
	H       int    `json:"h"`
	NativeW int    `json:"nativeW"`
	NativeH int    `json:"nativeH"`
	Error   string `json:"error"`
}

type BlobResult struct {
	B64   string `json:"b64"`
	W     int    `json:"w"`
	H     int    `json:"h"`
	Name  string `json:"name"`
	Error string `json:"error"`
}

type DragResult struct {
	URL   string `json:"url"` // relative URL served by the asset handler
	File  string `json:"file"`
	Name  string `json:"name"`
	Error string `json:"error"`
}

type BatchSummary struct {
	Total     int      `json:"total"`
	OK        int      `json:"ok"`
	Failed    int      `json:"failed"`
	Cancelled bool     `json:"cancelled"`
	OutDir    string   `json:"outDir"`
	ElapsedMs int64    `json:"elapsedMs"`
	Files     []string `json:"files"`
	Errors    []string `json:"errors"`
	Message   string   `json:"message"`
}

type WatchConfig struct {
	Inbox           string `json:"inbox"`
	Outbox          string `json:"outbox"`
	ProcessExisting bool   `json:"processExisting"`
}

type WatchStatus struct {
	Running   bool   `json:"running"`
	Inbox     string `json:"inbox"`
	Outbox    string `json:"outbox"`
	Processed int    `json:"processed"`
	Failed    int    `json:"failed"`
	Last      string `json:"last"`
	Error     string `json:"error"`
}

type UpdateInfo struct {
	Current         string `json:"current"`
	Latest          string `json:"latest"`
	UpdateAvailable bool   `json:"updateAvailable"`
	URL             string `json:"url"`
	Notes           string `json:"notes"`
	Error           string `json:"error"`
}

type Preset struct {
	Name     string   `json:"name"`
	Settings Settings `json:"settings"`
}

type appConfig struct {
	Last    *Settings         `json:"last,omitempty"`
	Presets []Preset          `json:"presets"`
	Prefs   map[string]string `json:"prefs"`
}

// ===========================================================================
// App
// ===========================================================================

type previewSrc struct {
	base    *image.NRGBA // flattened, source orientation, capped at previewMax
	nativeW int
	nativeH int
	exif    int
}

type frameEntry struct {
	key     string
	frame   *image.NRGBA
	angle   float64
	origURL string
}

type watcher struct {
	cancel context.CancelFunc
	done   chan struct{}
	cfg    WatchConfig
}

// App is the Wails-bound backend.
type App struct {
	ctx      context.Context
	dropOnce sync.Once

	mu     sync.Mutex // guards cache, order, frameC
	cache  map[string]*previewSrc
	order  []string
	frameC *frameEntry

	cfgMu   sync.Mutex
	cfg     appConfig
	cfgPath string

	batchMu      sync.Mutex
	batchRunning bool
	batchCancel  context.CancelFunc

	wMu       sync.Mutex
	w         *watcher
	wSettings Settings
	wOpts     ExportOptions
	wProc     int
	wFail     int
	wLast     string
	wErr      string
}

func NewApp() *App {
	return &App{cache: make(map[string]*previewSrc)}
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

func (a *App) OnStartup(ctx context.Context) {
	a.ctx = ctx
	debug.SetGCPercent(50) // keep the resident set small on 4 GB machines
	a.loadConfig()
	go cleanOldTemp()
}

func (a *App) OnDomReady(ctx context.Context) {
	if a.ctx == nil {
		a.ctx = ctx
	}
	// OnDomReady can fire again on a reload; register the drop handler only once.
	a.dropOnce.Do(func() {
		wailsrt.OnFileDrop(ctx, func(x, y int, paths []string) {
			defer func() { _ = recover() }()
			resolved := collectImages(paths, maxDropImages)
			wailsrt.EventsEmit(a.ctx, "files-dropped", resolved)
		})
	})
}

func (a *App) onShutdown(ctx context.Context) {
	a.StopWatch()
	a.CancelBatch()
}

// GetVersion returns the app version string.
func (a *App) GetVersion() string { return appVersion }

// GetStartupFiles returns image files passed on the command line ("Open with Photo BG").
func (a *App) GetStartupFiles() []string {
	var raw []string
	for _, arg := range os.Args[1:] {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		raw = append(raw, arg)
	}
	if len(raw) == 0 {
		return []string{}
	}
	return collectImages(raw, maxDropImages)
}

// GetDefaultFolder intentionally returns "" so the app always opens clean.
func (a *App) GetDefaultFolder() string { return "" }

// ---------------------------------------------------------------------------
// Settings defaults, normalization
// ---------------------------------------------------------------------------

func defaultSettings() Settings {
	return Settings{
		Ink:          "original",
		Separator:    "all",
		Transparency: 55,
		Boldness:     50,
		Stroke:       0,
		Despeckle:    4,
		AutoTrim:     true,
		Padding:      12,
		Upscale:      3,
		SizeMode:     "hd",
		Watermark: Watermark{
			Text:    "For Bank KYC Verification Only • {date}",
			Opacity: 30,
			Color:   "#000000",
			Size:    16,
			Mode:    "diagonal",
		},
	}
}

// GetDefaultSettings is used by the "Reset Defaults" button.
func (a *App) GetDefaultSettings() Settings { return defaultSettings() }

func parseHex(s string) (r, g, b uint8, ok bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(s) == 3 {
		s = string([]byte{s[0], s[0], s[1], s[1], s[2], s[2]})
	}
	if len(s) != 6 {
		return 0, 0, 0, false
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0, 0, 0, false
	}
	return uint8(v >> 16), uint8(v >> 8), uint8(v), true
}

func clampF(v, lo, hi float64) float64 {
	if math.IsNaN(v) {
		return lo
	}
	return math.Max(lo, math.Min(hi, v))
}

func normalizeSettings(s Settings) Settings {
	ink := strings.ToLower(strings.TrimSpace(s.Ink))
	switch ink {
	case "", "original", "orig":
		s.Ink = "original"
	case "black":
		s.Ink = "#000000"
	case "blue":
		s.Ink = "#0B3D91"
	case "red":
		s.Ink = "#C8102E"
	default:
		if r, g, b, ok := parseHex(ink); ok {
			s.Ink = fmt.Sprintf("#%02X%02X%02X", r, g, b)
		} else {
			s.Ink = "original"
		}
	}
	switch strings.ToLower(s.Separator) {
	case "signature", "stamp":
		s.Separator = strings.ToLower(s.Separator)
	default:
		s.Separator = "all"
	}
	s.Transparency = clampInt(s.Transparency, 0, 100)
	s.Boldness = clampInt(s.Boldness, 0, 100)
	s.Stroke = clampInt(s.Stroke, -2, 3)
	s.Despeckle = clampInt(s.Despeckle, 0, 10)
	s.Padding = clampInt(s.Padding, 0, 64)
	s.Upscale = clampInt(s.Upscale, 1, 3)
	r := ((s.Rotate % 360) + 360) % 360
	s.Rotate = (r / 90) * 90
	switch s.SizeMode {
	case "kb500", "mb1":
	default:
		s.SizeMode = "hd"
	}
	if s.Crop != nil {
		c := *s.Crop
		c.X = clampF(c.X, 0, 1)
		c.Y = clampF(c.Y, 0, 1)
		c.W = clampF(c.W, 0, 1-c.X)
		c.H = clampF(c.H, 0, 1-c.Y)
		if c.W < 0.005 || c.H < 0.005 || (c.X == 0 && c.Y == 0 && c.W >= 0.9999 && c.H >= 0.9999) {
			s.Crop = nil
		} else {
			s.Crop = &c
		}
	}
	// strokes: validate and cap total point count
	clean := make([]Stroke, 0, len(s.Strokes))
	total := 0
	for _, st := range s.Strokes {
		switch st.Tool {
		case "erase", "restore", "blackout", "pixelate":
		default:
			continue
		}
		n := len(st.Pts) / 2
		if n == 0 || total+n > maxStrokePoints {
			continue
		}
		total += n
		pts := make([]float64, n*2)
		for i := 0; i < n*2; i++ {
			pts[i] = clampF(st.Pts[i], -0.5, 1.5)
		}
		clean = append(clean, Stroke{Tool: st.Tool, Size: clampF(st.Size, 0.002, 0.5), Pts: pts})
	}
	s.Strokes = clean
	// watermark
	wm := s.Watermark
	wm.Opacity = clampInt(wm.Opacity, 1, 100)
	wm.Size = clampInt(wm.Size, 6, 80)
	if r, g, b, ok := parseHex(wm.Color); ok {
		wm.Color = fmt.Sprintf("#%02X%02X%02X", r, g, b)
	} else {
		wm.Color = "#000000"
	}
	if wm.Mode != "bottom" {
		wm.Mode = "diagonal"
	}
	if rs := []rune(wm.Text); len(rs) > 140 {
		wm.Text = string(rs[:140])
	}
	if strings.TrimSpace(wm.Text) == "" {
		wm.Enabled = false
	}
	s.Watermark = wm
	return s
}

func normalizeOptions(o ExportOptions) ExportOptions {
	if o.Kind != "document" {
		o.Kind = "ink"
	}
	seen := map[string]bool{}
	var fm []string
	for _, f := range o.Formats {
		f = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(f), "."))
		if f == "jpeg" {
			f = "jpg"
		}
		switch f {
		case "png", "svg", "pdf", "jpg":
			if !seen[f] {
				seen[f] = true
				fm = append(fm, f)
			}
		}
	}
	if o.Kind == "document" {
		var keep []string
		for _, f := range fm {
			if f != "svg" {
				keep = append(keep, f)
			}
		}
		fm = keep
		o.Split = false
	}
	if len(fm) == 0 {
		if o.Kind == "document" {
			fm = []string{"jpg"}
		} else {
			fm = []string{"png"}
		}
	}
	o.Formats = fm
	if o.PDFMode != "auto" {
		o.PDFMode = "a4"
	}
	o.OutDir = strings.TrimSpace(o.OutDir)
	return o
}

// stripImageSpecific removes per-image state (crop, strokes, rotation) so a settings
// object can be stored as a preset / used for batches.
func stripImageSpecific(s Settings) Settings {
	s.Crop = nil
	s.Strokes = nil
	s.Rotate = 0
	s.FlipH, s.FlipV = false, false
	return s
}

// ---------------------------------------------------------------------------
// Persistent configuration (last-used settings, presets, preferences)
// ---------------------------------------------------------------------------

func (a *App) loadConfig() {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "PhotoBG")
	_ = os.MkdirAll(dir, 0o755)
	a.cfgPath = filepath.Join(dir, "config.json")
	a.cfg = appConfig{Prefs: map[string]string{}}
	if data, err := os.ReadFile(a.cfgPath); err == nil {
		var c appConfig
		if json.Unmarshal(data, &c) == nil {
			a.cfg = c
		}
	}
	if a.cfg.Prefs == nil {
		a.cfg.Prefs = map[string]string{}
	}
}

// saveConfigLocked writes the config atomically (temp file + rename). Caller holds cfgMu.
func (a *App) saveConfigLocked() error {
	if a.cfgPath == "" {
		return errors.New("config path not ready")
	}
	data, err := json.MarshalIndent(a.cfg, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(a.cfgPath, data)
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".pbg_*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(name)
		if werr != nil {
			return werr
		}
		return cerr
	}
	_ = os.Chmod(name, 0o644)
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// LoadLastSettings returns the persisted last-used settings (or defaults).
func (a *App) LoadLastSettings() Settings {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	if a.cfg.Last == nil {
		return defaultSettings()
	}
	return normalizeSettings(*a.cfg.Last)
}

// SaveLastSettings persists the last-used settings (image-specific parts are dropped).
func (a *App) SaveLastSettings(s Settings) string {
	s = stripImageSpecific(normalizeSettings(s))
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	a.cfg.Last = &s
	if err := a.saveConfigLocked(); err != nil {
		return "Error: " + err.Error()
	}
	return "OK"
}

func (a *App) ListPresets() []Preset {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	out := make([]Preset, len(a.cfg.Presets))
	copy(out, a.cfg.Presets)
	return out
}

func (a *App) SavePreset(name string, s Settings) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "Error: Preset name is empty"
	}
	if rs := []rune(name); len(rs) > 48 {
		name = string(rs[:48])
	}
	s = stripImageSpecific(normalizeSettings(s))
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	replaced := false
	for i := range a.cfg.Presets {
		if strings.EqualFold(a.cfg.Presets[i].Name, name) {
			a.cfg.Presets[i] = Preset{Name: name, Settings: s}
			replaced = true
			break
		}
	}
	if !replaced {
		a.cfg.Presets = append(a.cfg.Presets, Preset{Name: name, Settings: s})
	}
	if err := a.saveConfigLocked(); err != nil {
		return "Error: " + err.Error()
	}
	return "OK"
}

func (a *App) DeletePreset(name string) string {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	kept := a.cfg.Presets[:0:0]
	for _, p := range a.cfg.Presets {
		if !strings.EqualFold(p.Name, strings.TrimSpace(name)) {
			kept = append(kept, p)
		}
	}
	a.cfg.Presets = kept
	if err := a.saveConfigLocked(); err != nil {
		return "Error: " + err.Error()
	}
	return "OK"
}

// GetPrefs / SetPref persist small UI preferences (language, theme, backdrop...).
func (a *App) GetPrefs() map[string]string {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	out := make(map[string]string, len(a.cfg.Prefs))
	for k, v := range a.cfg.Prefs {
		out[k] = v
	}
	return out
}

func (a *App) SetPref(key, value string) string {
	key = strings.TrimSpace(key)
	if key == "" || len(key) > 64 || len(value) > 512 {
		return "Error: invalid preference"
	}
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	if a.cfg.Prefs == nil {
		a.cfg.Prefs = map[string]string{}
	}
	a.cfg.Prefs[key] = value
	if err := a.saveConfigLocked(); err != nil {
		return "Error: " + err.Error()
	}
	return "OK"
}

// ---------------------------------------------------------------------------
// Temp / drag directories
// ---------------------------------------------------------------------------

func appTempDir(sub string) string {
	d := filepath.Join(os.TempDir(), tempDirName, sub)
	_ = os.MkdirAll(d, 0o755)
	return d
}

func cleanOldTemp() {
	defer func() { _ = recover() }()
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, sub := range []string{"paste", "drag"} {
		dir := appTempDir(sub)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if info, err := e.Info(); err == nil && !e.IsDir() && info.ModTime().Before(cutoff) {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
}

func isUnder(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

// assetHandler serves temporary files for native drag-out (/photobg-drag/<name>).
func (a *App) assetHandler() http.Handler {
	const prefix = "/photobg-drag/"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		name := filepath.Base(filepath.FromSlash(strings.TrimPrefix(r.URL.Path, prefix)))
		ext := strings.ToLower(filepath.Ext(name))
		if name == "." || name == string(filepath.Separator) || (ext != ".png" && ext != ".jpg" && ext != ".svg" && ext != ".pdf") {
			http.NotFound(w, r)
			return
		}
		p := filepath.Join(appTempDir("drag"), name)
		if !fileExists(p) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Disposition", "attachment; filename=\""+name+"\"")
		http.ServeFile(w, r, p)
	})
}

// ===========================================================================
// Path / file selection (recursive)
// ===========================================================================

func (a *App) SelectFolder() []string {
	dir, err := wailsrt.OpenDirectoryDialog(a.ctx, wailsrt.OpenDialogOptions{
		Title: "Select a Folder of Photos",
	})
	if err != nil || dir == "" {
		return []string{}
	}
	return collectImages([]string{dir}, maxBatchFiles)
}

func (a *App) SelectFiles() []string {
	files, err := wailsrt.OpenMultipleFilesDialog(a.ctx, wailsrt.OpenDialogOptions{
		Title: "Select Photos",
		Filters: []wailsrt.FileFilter{{
			DisplayName: "Images (*.png;*.jpg;*.jpeg;*.gif;*.bmp;*.webp;*.tif;*.tiff)",
			Pattern:     "*.png;*.jpg;*.jpeg;*.gif;*.bmp;*.webp;*.tif;*.tiff",
		}},
	})
	if err != nil || len(files) == 0 {
		return []string{}
	}
	return collectImages(files, 0)
}

// PickDirectory opens a folder chooser and returns the chosen path ("" if cancelled).
func (a *App) PickDirectory(title string) string {
	if strings.TrimSpace(title) == "" {
		title = "Select Folder"
	}
	dir, err := wailsrt.OpenDirectoryDialog(a.ctx, wailsrt.OpenDialogOptions{Title: title})
	if err != nil {
		return ""
	}
	return dir
}

// ResolvePath accepts a pasted path (quotes/whitespace tolerated). A folder expands to
// its sorted images (recursively); a valid image file returns itself; else [].
func (a *App) ResolvePath(inputPath string) []string {
	return collectImages([]string{inputPath}, maxBatchFiles)
}

func isImageExt(p string) bool {
	return imageExts[strings.ToLower(filepath.Ext(p))]
}

func cleanInput(s string) string {
	s = strings.TrimSpace(s)
	for len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'')) {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

func normalizePath(raw string) string {
	p := cleanInput(raw)
	if p == "" {
		return ""
	}
	p = filepath.Clean(p)
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// listImages returns the sorted, valid image files directly inside dir (non-recursive).
func listImages(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []string{}
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") || !isImageExt(name) {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i]) < strings.ToLower(out[j])
	})
	return out
}

// walkImages returns every image below dir (recursive, depth-limited, sorted),
// skipping hidden folders and the app's own output folders.
func walkImages(dir string, limit int) []string {
	var out []string
	root := filepath.Clean(dir)
	rootDepth := strings.Count(root, string(filepath.Separator))
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p == root {
				return nil
			}
			name := d.Name()
			if strings.HasPrefix(name, ".") || name == outDirName {
				return fs.SkipDir
			}
			if strings.Count(p, string(filepath.Separator))-rootDepth > maxFolderDepth {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") || !isImageExt(d.Name()) {
			return nil
		}
		out = append(out, p)
		if limit > 0 && len(out) >= limit {
			return io.EOF
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i]) < strings.ToLower(out[j])
	})
	return out
}

// collectImages expands files and folders (recursively) into unique absolute image paths.
// limit <= 0 means no cap.
func collectImages(paths []string, limit int) []string {
	out := make([]string, 0, len(paths))
	seen := make(map[string]struct{})
	add := func(p string) bool { // false once the cap is reached
		if _, dup := seen[p]; dup {
			return true
		}
		seen[p] = struct{}{}
		out = append(out, p)
		return limit <= 0 || len(out) < limit
	}
	for _, raw := range paths {
		p := normalizePath(raw)
		if p == "" {
			continue
		}
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if fi.IsDir() {
			rem := 0
			if limit > 0 {
				rem = limit - len(out)
			}
			for _, f := range walkImages(p, rem) {
				if !add(f) {
					return out
				}
			}
		} else if isImageExt(p) {
			if !add(p) {
				return out
			}
		}
	}
	return out
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// baseDir: basePath itself if it is a directory, otherwise its parent directory.
func baseDir(basePath string) string {
	p := normalizePath(basePath)
	if p == "" {
		return ""
	}
	if fi, err := os.Stat(p); err == nil && fi.IsDir() {
		return p
	}
	return filepath.Dir(p)
}

// resolveOutDir picks the output folder: explicit > <photo folder>/PhotoBG_Output;
// photos that live in the app's temp area (paste/demo) go to ~/Pictures/PhotoBG_Output.
func resolveOutDir(srcPath, explicit string) string {
	if e := normalizePath(explicit); e != "" {
		return e
	}
	if srcPath == "" || isUnder(srcPath, filepath.Join(os.TempDir(), tempDirName)) {
		return picturesOutDir()
	}
	return filepath.Join(filepath.Dir(srcPath), outDirName)
}

// ===========================================================================
// Image helpers
// ===========================================================================

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func imageDims(path string) (int, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0, errors.New("Unsupported or damaged image file")
	}
	if cfg.Width < 1 || cfg.Height < 1 || int64(cfg.Width)*int64(cfg.Height) > maxDecodePixels {
		return 0, 0, errors.New("Image dimensions are too large")
	}
	return cfg.Width, cfg.Height, nil
}

func decodeFile(path string) (image.Image, error) {
	if _, _, err := imageDims(path); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(bufio.NewReaderSize(f, 1<<16))
	if err != nil {
		return nil, errors.New("Could not decode image")
	}
	return img, nil
}

type opaquer interface{ Opaque() bool }

func isOpaque(img image.Image) bool {
	o, ok := img.(opaquer)
	return ok && o.Opaque()
}

// flatten composites img over white and returns it as an opaque NRGBA at (0,0).
func flatten(src image.Image) *image.NRGBA {
	b := src.Bounds()
	if n, ok := src.(*image.NRGBA); ok && b.Min == (image.Point{}) && n.Opaque() {
		return n
	}
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	for i := range dst.Pix {
		dst.Pix[i] = 0xff
	}
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Over)
	// Fully opaque, so RGBA and NRGBA share the same byte layout.
	return &image.NRGBA{Pix: dst.Pix, Stride: dst.Stride, Rect: dst.Rect}
}

// scaleInto scales src into rectangle dr of a new cw x ch canvas and returns it as NRGBA.
// Scaling happens in the fast premultiplied RGBA path and is un-premultiplied afterwards.
func scaleInto(src image.Image, dr image.Rectangle, cw, ch int, k draw.Scaler) *image.NRGBA {
	tmp := image.NewRGBA(image.Rect(0, 0, cw, ch))
	k.Scale(tmp, dr, src, src.Bounds(), draw.Src, nil)

	out := image.NewNRGBA(tmp.Rect)
	sp, dp := tmp.Pix, out.Pix
	for i := 0; i+3 < len(sp); i += 4 {
		a := uint32(sp[i+3])
		switch a {
		case 0:
			continue
		case 255:
			copy(dp[i:i+4], sp[i:i+4])
		default:
			for c := 0; c < 3; c++ {
				v := (uint32(sp[i+c])*255 + a/2) / a
				if v > 255 {
					v = 255
				}
				dp[i+c] = uint8(v)
			}
			dp[i+3] = uint8(a)
		}
	}
	return out
}

func fitDims(w, h, limit int) (int, int) {
	if w <= limit && h <= limit {
		return w, h
	}
	if w >= h {
		nh := int(math.Round(float64(h) * float64(limit) / float64(w)))
		return limit, max(1, nh)
	}
	nw := int(math.Round(float64(w) * float64(limit) / float64(h)))
	return max(1, nw), limit
}

func pngDataURL(img image.Image) (string, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, img); err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

func asOpaqueRGBA(img *image.NRGBA) *image.RGBA {
	// Callers guarantee img is opaque, so NRGBA and RGBA share the same byte layout.
	return &image.RGBA{Pix: img.Pix, Stride: img.Stride, Rect: img.Rect}
}

func jpegBytes(img *image.NRGBA, quality int) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, asOpaqueRGBA(flatten(img)), &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func jpegDataURL(img *image.NRGBA, quality int) (string, error) {
	b, err := jpegBytes(img, quality)
	if err != nil {
		return "", err
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(b), nil
}

// pixelBudget bounds how many native pixels are decoded at the same time in a batch.
type pixelBudget struct {
	mu   sync.Mutex
	cond *sync.Cond
	used int64
	cap  int64
}

func newPixelBudget(cap int64) *pixelBudget {
	b := &pixelBudget{cap: cap}
	b.cond = sync.NewCond(&b.mu)
	return b
}

func (b *pixelBudget) acquire(n int64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	for b.used > 0 && b.used+n > b.cap {
		b.cond.Wait()
	}
	b.used += n
	b.mu.Unlock()
}

func (b *pixelBudget) release(n int64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.used -= n
	b.mu.Unlock()
	b.cond.Broadcast()
}

// ===========================================================================
// EXIF orientation + rotate / flip / crop at native resolution
// ===========================================================================

// exifOrientation returns the EXIF orientation tag (1..8) of a JPEG, or 1.
func exifOrientation(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 1
	}
	defer f.Close()
	buf := make([]byte, 192<<10)
	n, _ := io.ReadFull(f, buf)
	buf = buf[:n]
	if n < 4 || buf[0] != 0xFF || buf[1] != 0xD8 {
		return 1
	}
	i := 2
	for i+4 <= len(buf) {
		if buf[i] != 0xFF {
			i++
			continue
		}
		m := buf[i+1]
		switch {
		case m == 0xFF:
			i++
			continue
		case m == 0x01 || (m >= 0xD0 && m <= 0xD8):
			i += 2
			continue
		case m == 0xDA || m == 0xD9:
			return 1
		}
		l := int(binary.BigEndian.Uint16(buf[i+2:]))
		end := i + 2 + l
		if l < 2 || end > len(buf) {
			return 1
		}
		if m == 0xE1 && l >= 8 && string(buf[i+4:i+10]) == "Exif\x00\x00" {
			return parseTIFFOrientation(buf[i+10 : end])
		}
		i = end
	}
	return 1
}

func parseTIFFOrientation(t []byte) int {
	if len(t) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	if bo.Uint16(t[2:]) != 42 {
		return 1
	}
	off := int(bo.Uint32(t[4:]))
	if off < 8 || off+2 > len(t) {
		return 1
	}
	n := int(bo.Uint16(t[off:]))
	for k := 0; k < n; k++ {
		e := off + 2 + k*12
		if e+12 > len(t) {
			break
		}
		if bo.Uint16(t[e:]) == 0x0112 {
			v := int(bo.Uint16(t[e+8:]))
			if v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
	}
	return 1
}

// orient is "rotate clockwise by rot degrees, then flip".
type orient struct {
	rot    int
	fh, fv bool
}

func (o orient) identity() bool { return o.rot == 0 && !o.fh && !o.fv }
func (o orient) swaps() bool    { return o.rot == 90 || o.rot == 270 }

func (o orient) inv(u, v float64) (float64, float64) {
	if o.fv {
		v = 1 - v
	}
	if o.fh {
		u = 1 - u
	}
	switch o.rot {
	case 90:
		u, v = v, 1-u
	case 180:
		u, v = 1-u, 1-v
	case 270:
		u, v = 1-v, u
	}
	return u, v
}

func exifToOrient(n int) orient {
	switch n {
	case 2:
		return orient{fh: true}
	case 3:
		return orient{rot: 180}
	case 4:
		return orient{fv: true}
	case 5:
		return orient{rot: 90, fh: true}
	case 6:
		return orient{rot: 90}
	case 7:
		return orient{rot: 90, fv: true}
	case 8:
		return orient{rot: 270}
	}
	return orient{}
}

func orientsFor(exif int, s Settings) []orient {
	var out []orient
	if o := exifToOrient(exif); !o.identity() {
		out = append(out, o)
	}
	if o := (orient{rot: s.Rotate, fh: s.FlipH, fv: s.FlipV}); !o.identity() {
		out = append(out, o)
	}
	return out
}

func orientedDims(w, h int, ors []orient) (int, int) {
	for _, o := range ors {
		if o.swaps() {
			w, h = h, w
		}
	}
	return w, h
}

// applyOrient rotates (clockwise) and flips a small NRGBA.
func applyOrient(src *image.NRGBA, o orient) *image.NRGBA {
	if o.identity() {
		return src
	}
	sw, sh := src.Rect.Dx(), src.Rect.Dy()
	dw, dh := sw, sh
	if o.swaps() {
		dw, dh = sh, sw
	}
	dst := image.NewNRGBA(image.Rect(0, 0, dw, dh))
	for dy := 0; dy < dh; dy++ {
		for dx := 0; dx < dw; dx++ {
			fx, fy := dx, dy
			if o.fh {
				fx = dw - 1 - fx
			}
			if o.fv {
				fy = dh - 1 - fy
			}
			var sx, sy int
			switch o.rot {
			case 90:
				sx, sy = fy, sh-1-fx
			case 180:
				sx, sy = sw-1-fx, sh-1-fy
			case 270:
				sx, sy = sw-1-fy, fx
			default:
				sx, sy = fx, fy
			}
			si := src.PixOffset(sx, sy)
			di := dst.PixOffset(dx, dy)
			copy(dst.Pix[di:di+4], src.Pix[si:si+4])
		}
	}
	return dst
}

type subImager interface {
	SubImage(r image.Rectangle) image.Image
}

// orientCrop applies orientations then a normalized crop (expressed in the final frame),
// but does the cropping on the ORIGINAL pixels first so a 33 MP scan is never rotated
// at full size. The result is an opaque NRGBA whose longest side is <= capDim.
func orientCrop(src image.Image, orients []orient, crop *CropRect, capDim int) (*image.NRGBA, error) {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw < 1 || sh < 1 {
		return nil, errors.New("Image is empty")
	}
	x0, y0, x1, y1 := 0.0, 0.0, 1.0, 1.0
	if crop != nil {
		ax, ay := crop.X, crop.Y
		bx, by := crop.X+crop.W, crop.Y+crop.H
		for i := len(orients) - 1; i >= 0; i-- {
			ax, ay = orients[i].inv(ax, ay)
			bx, by = orients[i].inv(bx, by)
		}
		x0, x1 = math.Min(ax, bx), math.Max(ax, bx)
		y0, y1 = math.Min(ay, by), math.Max(ay, by)
	}
	px0 := clampInt(int(math.Round(x0*float64(sw))), 0, sw-1)
	py0 := clampInt(int(math.Round(y0*float64(sh))), 0, sh-1)
	px1 := clampInt(int(math.Round(x1*float64(sw))), px0+1, sw)
	py1 := clampInt(int(math.Round(y1*float64(sh))), py0+1, sh)
	rect := image.Rect(b.Min.X+px0, b.Min.Y+py0, b.Min.X+px1, b.Min.Y+py1)

	var sub image.Image = src
	if si, ok := src.(subImager); ok {
		sub = si.SubImage(rect)
	} else {
		tmp := image.NewNRGBA(image.Rect(0, 0, rect.Dx(), rect.Dy()))
		draw.Draw(tmp, tmp.Bounds(), src, rect.Min, draw.Src)
		sub = tmp
	}
	cw, ch := sub.Bounds().Dx(), sub.Bounds().Dy()
	nw, nh := fitDims(cw, ch, capDim)

	var out *image.NRGBA
	if nw == cw && nh == ch {
		if n, ok := sub.(*image.NRGBA); ok && n.Opaque() && n.Rect.Min == (image.Point{}) {
			out = n
		} else {
			tmp := image.NewNRGBA(image.Rect(0, 0, cw, ch))
			draw.Draw(tmp, tmp.Bounds(), sub, sub.Bounds().Min, draw.Src)
			out = flatten(tmp)
		}
	} else {
		out = flatten(scaleInto(sub, image.Rect(0, 0, nw, nh), nw, nh, draw.BiLinear))
	}
	for _, o := range orients {
		out = applyOrient(out, o)
	}
	return out, nil
}

// ===========================================================================
// Deskew
// ===========================================================================

// detectSkew returns the clockwise tilt (degrees) of the dominant text/ink baseline
// using a projection-profile search over -15..+15 degrees. 0 means "leave alone".
func detectSkew(frame *image.NRGBA) float64 {
	ang, ratio := detectSkewScore(frame)
	if ratio < skewMinGain {
		return 0
	}
	return ang
}

// skewMinGain is how much sharper the best row-projection must be than the unrotated one.
// Real text lines gain far more than this; free-hand signatures usually do not, so they
// are left untouched instead of being rotated by a guess.
const skewMinGain = 1.4

func detectSkewScore(frame *image.NRGBA) (float64, float64) {
	w, h := frame.Rect.Dx(), frame.Rect.Dy()
	sw, sh := fitDims(w, h, 480)
	small := frame
	if sw != w || sh != h {
		small = scaleInto(frame, image.Rect(0, 0, sw, sh), sw, sh, draw.BiLinear)
	}
	alpha := computeAlpha(small, 50, 50, "all")
	xs := make([]float32, 0, 4096)
	ys := make([]float32, 0, 4096)
	for y := 0; y < sh; y++ {
		for x := 0; x < sw; x++ {
			if alpha[y*sw+x] > 90 {
				xs = append(xs, float32(x))
				ys = append(ys, float32(y))
			}
		}
	}
	if len(xs) > 150000 { // thin out dense scans
		step := len(xs)/150000 + 1
		k := 0
		for i := 0; i < len(xs); i += step {
			xs[k], ys[k] = xs[i], ys[i]
			k++
		}
		xs, ys = xs[:k], ys[:k]
	}
	if len(xs) < 40 {
		return 0, 0
	}
	bins := make([]int32, sw+sh+8)
	score := func(deg float64) float64 {
		for i := range bins {
			bins[i] = 0
		}
		rad := deg * math.Pi / 180
		c, s := float32(math.Cos(rad)), float32(math.Sin(rad))
		off := float32(sw + 2)
		for i := range xs {
			yp := ys[i]*c - xs[i]*s + off
			bi := int(yp)
			if bi >= 0 && bi < len(bins) {
				bins[bi]++
			}
		}
		var sum float64
		for _, v := range bins {
			sum += float64(v) * float64(v)
		}
		return sum
	}
	base := score(0)
	best, bestScore := 0.0, base
	for d := -15.0; d <= 15.0; d += 0.5 {
		if sc := score(d); sc > bestScore {
			best, bestScore = d, sc
		}
	}
	for d := best - 0.5; d <= best+0.5; d += 0.1 {
		if sc := score(d); sc > bestScore {
			best, bestScore = d, sc
		}
	}
	if base <= 0 || math.Abs(best) < 0.2 {
		return 0, 0
	}
	return math.Round(best*10) / 10, bestScore / base
}

// rotateNRGBA rotates about the centre so that a baseline tilted by deg becomes horizontal.
// The canvas keeps its size; uncovered corners are filled with white.
func rotateNRGBA(src *image.NRGBA, deg float64) *image.NRGBA {
	w, h := src.Rect.Dx(), src.Rect.Dy()
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	rad := deg * math.Pi / 180
	c, s := math.Cos(rad), math.Sin(rad)
	cx, cy := float64(w-1)/2, float64(h-1)/2
	sp := src.Pix
	for y := 0; y < h; y++ {
		dy := float64(y) - cy
		for x := 0; x < w; x++ {
			dx := float64(x) - cx
			fx := dx*c - dy*s + cx
			fy := dx*s + dy*c + cy
			di := dst.PixOffset(x, y)
			if fx < 0 || fy < 0 || fx > float64(w-1) || fy > float64(h-1) {
				dst.Pix[di], dst.Pix[di+1], dst.Pix[di+2], dst.Pix[di+3] = 255, 255, 255, 255
				continue
			}
			x0, y0 := int(fx), int(fy)
			x1, y1 := min(x0+1, w-1), min(y0+1, h-1)
			tx, ty := float32(fx-float64(x0)), float32(fy-float64(y0))
			i00, i10 := src.PixOffset(x0, y0), src.PixOffset(x1, y0)
			i01, i11 := src.PixOffset(x0, y1), src.PixOffset(x1, y1)
			for k := 0; k < 3; k++ {
				top := float32(sp[i00+k])*(1-tx) + float32(sp[i10+k])*tx
				bot := float32(sp[i01+k])*(1-tx) + float32(sp[i11+k])*tx
				dst.Pix[di+k] = uint8(top*(1-ty) + bot*ty + 0.5)
			}
			dst.Pix[di+3] = 255
		}
	}
	return dst
}

// buildFrame = orient + native crop + cap + optional auto-deskew. The frame is what
// the user sees as "Original" and the coordinate space of all brush strokes.
func buildFrame(src image.Image, orients []orient, s Settings, capDim int) (*image.NRGBA, float64, error) {
	f, err := orientCrop(src, orients, s.Crop, capDim)
	if err != nil {
		return nil, 0, err
	}
	angle := 0.0
	if s.Deskew {
		angle = detectSkew(f)
		if angle != 0 {
			f = rotateNRGBA(f, angle)
		}
	}
	return f, angle, nil
}

// ===========================================================================
// Transparency engine (integral image) + stamp / signature separator
// ===========================================================================

func lumPlane(img *image.NRGBA) []uint8 {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	pix, stride := img.Pix, img.Stride
	lum := make([]uint8, w*h)
	for y := 0; y < h; y++ {
		row := pix[y*stride : y*stride+w*4]
		o := y * w
		for x := 0; x < w; x++ {
			i := x * 4
			lum[o+x] = uint8((299*uint32(row[i]) + 587*uint32(row[i+1]) + 114*uint32(row[i+2]) + 500) / 1000)
		}
	}
	return lum
}

// satTable builds the summed-area table (integral image) for O(1) window means.
func satTable(lum []uint8, w, h int) []uint32 {
	sw := w + 1
	sat := make([]uint32, sw*(h+1))
	for y := 0; y < h; y++ {
		var rowSum uint32
		o := y * w
		above := y * sw
		cur := (y + 1) * sw
		for x := 0; x < w; x++ {
			rowSum += uint32(lum[o+x])
			sat[cur+x+1] = sat[above+x+1] + rowSum
		}
	}
	return sat
}

func bgRadiusFor(w, h int) int {
	return clampInt(int(math.Round(float64(max(w, h))*0.0375)), 20, 120)
}

func smoothRamp(v, lo, hi float32) float32 {
	if v <= lo {
		return 0
	}
	if v >= hi {
		return 1
	}
	return (v - lo) / (hi - lo)
}

// inkClassWeights returns how much a pixel looks like pen ink (black / blue) and
// like official-stamp ink (red / crimson / purple / magenta), each 0..1.
func inkClassWeights(r, g, b uint8) (sig, stamp float32) {
	mx, mn := r, r
	if g > mx {
		mx = g
	}
	if b > mx {
		mx = b
	}
	if g < mn {
		mn = g
	}
	if b < mn {
		mn = b
	}
	c := float32(mx - mn)
	cs := smoothRamp(c, 16, 42)
	if cs == 0 {
		return 1, 0
	}
	var hue float32
	fr, fg, fb := float32(r), float32(g), float32(b)
	switch mx {
	case r:
		hue = 60 * float32(math.Mod(float64((fg-fb)/c), 6))
	case g:
		hue = 60 * ((fb-fr)/c + 2)
	default:
		hue = 60 * ((fr-fg)/c + 4)
	}
	if hue < 0 {
		hue += 360
	}
	var stampH, blueH float32
	switch {
	case hue >= 300 || hue <= 18:
		stampH = 1
	case hue < 38:
		stampH = (38 - hue) / 20
	case hue > 262 && hue < 300:
		stampH = (hue - 262) / 38
	}
	switch {
	case hue >= 185 && hue <= 255:
		blueH = 1
	case hue > 160 && hue < 185:
		blueH = (hue - 160) / 25
	case hue > 255 && hue < 275:
		blueH = (275 - hue) / 20
	}
	return (1 - cs) + cs*blueH, cs * stampH
}

// computeAlpha implements the adaptive background-normalisation alpha matte.
//
//	transparency 0..100: 0 keeps subtle tones, 100 removes background aggressively
//	boldness     0..100: edge contrast / threshold
//	sep:         "all" | "signature" | "stamp"
func computeAlpha(img *image.NRGBA, transparency, boldness int, sep string) []uint8 {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	pix, stride := img.Pix, img.Stride
	lum := lumPlane(img)
	sw := w + 1
	sat := satTable(lum, w, h)
	rad := bgRadiusFor(w, h)

	tf := float32(clampInt(transparency, 0, 100)) / 100
	bf := float32(clampInt(boldness, 0, 100)) / 100
	ceilL := 252 - tf*22
	minAlpha := 8 + tf*56
	thresh := 185 + bf*45
	span := ceilL - thresh
	if span < 1 {
		span = 1
	}

	alpha := make([]uint8, w*h)
	for y := 0; y < h; y++ {
		y0 := max(0, y-rad)
		y1 := min(h, y+rad+1)
		for x := 0; x < w; x++ {
			x0 := max(0, x-rad)
			x1 := min(w, x+rad+1)
			area := (x1 - x0) * (y1 - y0)
			sum := sat[y1*sw+x1] - sat[y0*sw+x1] - sat[y1*sw+x0] + sat[y0*sw+x0]
			mean := float32(sum) / float32(area)
			if mean < 1 {
				mean = 1
			}
			n := float32(lum[y*w+x]) * 255 / mean
			if n >= ceilL {
				continue
			}
			a := (ceilL - n) / span * 255
			if a > 255 {
				a = 255
			}
			if a < minAlpha {
				continue
			}
			a = (a - minAlpha) / (255 - minAlpha) * 255
			if sep != "all" {
				i := y*stride + x*4
				sig, stamp := inkClassWeights(pix[i], pix[i+1], pix[i+2])
				if sep == "signature" {
					a *= sig
				} else {
					a *= stamp
				}
			}
			alpha[y*w+x] = uint8(a + 0.5)
		}
	}
	return alpha
}

// morphAlpha dilates (thickens) or erodes (thins) the alpha plane by `steps` 3x3 steps.
func morphAlpha(a []uint8, w, h, steps int, dilate bool) {
	if steps <= 0 || w < 3 || h < 3 {
		return
	}
	tmp := make([]uint8, len(a))
	pick := func(p, q uint8) uint8 {
		if dilate {
			if q > p {
				return q
			}
			return p
		}
		if q < p {
			return q
		}
		return p
	}
	for s := 0; s < steps; s++ {
		for y := 0; y < h; y++ {
			row := a[y*w : y*w+w]
			t := tmp[y*w : y*w+w]
			for x := 0; x < w; x++ {
				v := row[x]
				if x > 0 {
					v = pick(v, row[x-1])
				}
				if x < w-1 {
					v = pick(v, row[x+1])
				}
				t[x] = v
			}
		}
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				v := tmp[y*w+x]
				if y > 0 {
					v = pick(v, tmp[(y-1)*w+x])
				}
				if y < h-1 {
					v = pick(v, tmp[(y+1)*w+x])
				}
				a[y*w+x] = v
			}
		}
	}
}

// despeckleAlpha removes small connected components (8-connectivity) = scanner dust.
// scale is longestSide/1000 so the filter behaves the same at preview and export size.
func despeckleAlpha(a []uint8, w, h, level int, scale float64) {
	if level <= 0 {
		return
	}
	minArea := int(math.Round(float64(level*level)*0.8*scale*scale)) + level
	visited := make([]bool, w*h)
	stack := make([]int32, 0, 1024)
	comp := make([]int32, 0, 1024)
	for start := 0; start < w*h; start++ {
		if a[start] == 0 || visited[start] {
			continue
		}
		stack = append(stack[:0], int32(start))
		comp = comp[:0]
		visited[start] = true
		area := 0
		for len(stack) > 0 {
			p := int(stack[len(stack)-1])
			stack = stack[:len(stack)-1]
			comp = append(comp, int32(p))
			if a[p] >= 16 {
				area++
			}
			px, py := p%w, p/w
			for dy := -1; dy <= 1; dy++ {
				ny := py + dy
				if ny < 0 || ny >= h {
					continue
				}
				for dx := -1; dx <= 1; dx++ {
					nx := px + dx
					if nx < 0 || nx >= w || (dx == 0 && dy == 0) {
						continue
					}
					q := ny*w + nx
					if a[q] != 0 && !visited[q] {
						visited[q] = true
						stack = append(stack, int32(q))
					}
				}
			}
		}
		if area < minArea {
			for _, p := range comp {
				a[p] = 0
			}
		}
	}
}

// ===========================================================================
// Brush strokes: eraser / restore / blackout / pixelate
// ===========================================================================

// strokeCoverage rasterises a stroke into a bbox-sized 0..255 coverage mask.
func strokeCoverage(st Stroke, w, h int) (cov []uint8, bx0, by0, bx1, by1 int) {
	n := len(st.Pts) / 2
	if n == 0 {
		return nil, 0, 0, 0, 0
	}
	r := math.Max(0.75, st.Size*float64(max(w, h))/2)
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	px := make([]float64, n)
	py := make([]float64, n)
	for i := 0; i < n; i++ {
		px[i] = st.Pts[2*i] * float64(w)
		py[i] = st.Pts[2*i+1] * float64(h)
		minX, maxX = math.Min(minX, px[i]), math.Max(maxX, px[i])
		minY, maxY = math.Min(minY, py[i]), math.Max(maxY, py[i])
	}
	bx0 = clampInt(int(math.Floor(minX-r-1)), 0, w)
	by0 = clampInt(int(math.Floor(minY-r-1)), 0, h)
	bx1 = clampInt(int(math.Ceil(maxX+r+1)), 0, w)
	by1 = clampInt(int(math.Ceil(maxY+r+1)), 0, h)
	bw, bh := bx1-bx0, by1-by0
	if bw <= 0 || bh <= 0 {
		return nil, 0, 0, 0, 0
	}
	cov = make([]uint8, bw*bh)
	stamp := func(cx, cy float64) {
		ys := clampInt(int(math.Floor(cy-r-1)), by0, by1)
		ye := clampInt(int(math.Ceil(cy+r+1)), by0, by1)
		xs := clampInt(int(math.Floor(cx-r-1)), bx0, bx1)
		xe := clampInt(int(math.Ceil(cx+r+1)), bx0, bx1)
		for y := ys; y < ye; y++ {
			dy := float64(y) + 0.5 - cy
			for x := xs; x < xe; x++ {
				dx := float64(x) + 0.5 - cx
				d := math.Sqrt(dx*dx + dy*dy)
				c := r + 0.5 - d
				if c <= 0 {
					continue
				}
				if c > 1 {
					c = 1
				}
				v := uint8(c*255 + 0.5)
				i := (y-by0)*bw + (x - bx0)
				if v > cov[i] {
					cov[i] = v
				}
			}
		}
	}
	step := math.Max(0.5, r*0.35)
	stamp(px[0], py[0])
	for i := 1; i < n; i++ {
		dx, dy := px[i]-px[i-1], py[i]-py[i-1]
		dist := math.Hypot(dx, dy)
		k := int(math.Ceil(dist / step))
		for j := 1; j <= k; j++ {
			t := float64(j) / float64(k)
			stamp(px[i-1]+dx*t, py[i-1]+dy*t)
		}
	}
	return cov, bx0, by0, bx1, by1
}

func hasTool(strokes []Stroke, tools ...string) bool {
	for _, s := range strokes {
		for _, t := range tools {
			if s.Tool == t {
				return true
			}
		}
	}
	return false
}

// applyAlphaStrokes applies eraser / restore strokes to the alpha plane in order.
func applyAlphaStrokes(alpha, raw []uint8, w, h int, strokes []Stroke) {
	for _, st := range strokes {
		if st.Tool != "erase" && st.Tool != "restore" {
			continue
		}
		cov, bx0, by0, bx1, by1 := strokeCoverage(st, w, h)
		if cov == nil {
			continue
		}
		bw := bx1 - bx0
		for y := by0; y < by1; y++ {
			for x := bx0; x < bx1; x++ {
				c := uint32(cov[(y-by0)*bw+(x-bx0)])
				if c == 0 {
					continue
				}
				i := y*w + x
				if st.Tool == "erase" {
					alpha[i] = uint8(uint32(alpha[i]) * (255 - c) / 255)
				} else if raw != nil && raw[i] > alpha[i] {
					alpha[i] = uint8(uint32(alpha[i]) + (uint32(raw[i])-uint32(alpha[i]))*c/255)
				}
			}
		}
	}
}

// applyRedactions applies blackout / pixelate strokes in order to an NRGBA image.
func applyRedactions(img *image.NRGBA, strokes []Stroke) {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	for _, st := range strokes {
		if st.Tool != "blackout" && st.Tool != "pixelate" {
			continue
		}
		cov, bx0, by0, bx1, by1 := strokeCoverage(st, w, h)
		if cov == nil {
			continue
		}
		bw := bx1 - bx0
		if st.Tool == "blackout" {
			for y := by0; y < by1; y++ {
				for x := bx0; x < bx1; x++ {
					c := float64(cov[(y-by0)*bw+(x-bx0)]) / 255
					if c > 0 {
						blendPixel(img.Pix[img.PixOffset(x, y):], 0, 0, 0, c)
					}
				}
			}
			continue
		}
		radius := math.Max(0.75, st.Size*float64(max(w, h))/2)
		block := max(4, int(radius*0.6))
		for gy := (by0 / block) * block; gy < by1; gy += block {
			for gx := (bx0 / block) * block; gx < bx1; gx += block {
				ex0, ey0 := max(gx, 0), max(gy, 0)
				ex1, ey1 := min(gx+block, w), min(gy+block, h)
				var sr, sg, sb, sa float64
				cnt := 0
				for y := ey0; y < ey1; y++ {
					for x := ex0; x < ex1; x++ {
						p := img.Pix[img.PixOffset(x, y):]
						a := float64(p[3]) / 255
						sr += float64(p[0]) * a
						sg += float64(p[1]) * a
						sb += float64(p[2]) * a
						sa += a
						cnt++
					}
				}
				if cnt == 0 {
					continue
				}
				ar, ag, ab, aa := sr/float64(cnt), sg/float64(cnt), sb/float64(cnt), sa/float64(cnt)
				for y := max(ey0, by0); y < min(ey1, by1); y++ {
					for x := max(ex0, bx0); x < min(ex1, bx1); x++ {
						c := float64(cov[(y-by0)*bw+(x-bx0)]) / 255
						if c == 0 {
							continue
						}
						p := img.Pix[img.PixOffset(x, y):]
						da := float64(p[3]) / 255
						dr, dg, db := float64(p[0])*da, float64(p[1])*da, float64(p[2])*da
						na := da + (aa-da)*c
						if na < 0.0005 {
							p[0], p[1], p[2], p[3] = 0, 0, 0, 0
							continue
						}
						nr := dr + (ar-dr)*c
						ng := dg + (ag-dg)*c
						nb := db + (ab-db)*c
						p[0] = uint8(math.Min(255, nr/na+0.5))
						p[1] = uint8(math.Min(255, ng/na+0.5))
						p[2] = uint8(math.Min(255, nb/na+0.5))
						p[3] = uint8(math.Min(255, na*255+0.5))
					}
				}
			}
		}
	}
}

// blendPixel composites colour (r,g,b) with opacity a (0..1) over a non-premultiplied NRGBA pixel.
func blendPixel(p []uint8, r, g, b uint8, a float64) {
	if a <= 0 {
		return
	}
	if a > 1 {
		a = 1
	}
	da := float64(p[3]) / 255
	oa := a + da*(1-a)
	if oa <= 0 {
		return
	}
	p[0] = uint8(math.Min(255, (float64(r)*a+float64(p[0])*da*(1-a))/oa+0.5))
	p[1] = uint8(math.Min(255, (float64(g)*a+float64(p[1])*da*(1-a))/oa+0.5))
	p[2] = uint8(math.Min(255, (float64(b)*a+float64(p[2])*da*(1-a))/oa+0.5))
	p[3] = uint8(math.Min(255, oa*255+0.5))
}

// ===========================================================================
// Ink layer renderer: alpha -> despeckle -> boldness -> brush -> colour -> trim
// ===========================================================================

func inkRGB(s Settings) (r, g, b uint8, orig bool) {
	if s.Ink == "original" {
		return 0, 0, 0, true
	}
	r, g, b, ok := parseHex(s.Ink)
	if !ok {
		return 0, 0, 0, true
	}
	return r, g, b, false
}

// renderInk turns an opaque frame into a transparent ink layer. When AutoTrim is on the
// layer is cropped to the ink bounding box plus padding*padScale; off is the position of
// the layer's top-left inside the frame (negative when padding extends past the frame).
func renderInk(frame *image.NRGBA, s Settings, padScale float64) (layer *image.NRGBA, off image.Point, empty bool) {
	w, h := frame.Rect.Dx(), frame.Rect.Dy()
	alpha := computeAlpha(frame, s.Transparency, s.Boldness, s.Separator)
	sc := float64(max(w, h)) / 1000

	despeckleAlpha(alpha, w, h, s.Despeckle, sc)
	if s.Stroke != 0 {
		steps := abs(s.Stroke) * max(1, int(sc+0.5))
		morphAlpha(alpha, w, h, steps, s.Stroke > 0)
	}
	if hasTool(s.Strokes, "erase", "restore") {
		var raw []uint8
		if hasTool(s.Strokes, "restore") {
			raw = computeAlpha(frame, 25, 100, s.Separator)
		}
		applyAlphaStrokes(alpha, raw, w, h, s.Strokes)
	}

	cr, cg, cb, orig := inkRGB(s)
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	op, fp := out.Pix, frame.Pix
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			a := alpha[y*w+x]
			if a == 0 {
				continue
			}
			o := (y*w + x) * 4
			if orig {
				si := y*frame.Stride + x*4
				op[o], op[o+1], op[o+2] = fp[si], fp[si+1], fp[si+2]
			} else {
				op[o], op[o+1], op[o+2] = cr, cg, cb
			}
			op[o+3] = a
		}
	}
	if hasTool(s.Strokes, "blackout", "pixelate") {
		applyRedactions(out, s.Strokes)
	}
	if !s.AutoTrim {
		return out, image.Point{}, !hasInk(out)
	}
	pad := 0
	if s.Padding > 0 {
		pad = max(1, int(math.Round(float64(s.Padding)*padScale)))
	}
	return trimPad(out, pad)
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func hasInk(img *image.NRGBA) bool {
	for i := 3; i < len(img.Pix); i += 4 {
		if img.Pix[i] >= 8 {
			return true
		}
	}
	return false
}

// trimPad crops img to its ink bounding box and adds a transparent margin of pad pixels.
func trimPad(img *image.NRGBA, pad int) (*image.NRGBA, image.Point, bool) {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	minX, minY, maxX, maxY := w, h, -1, -1
	for y := 0; y < h; y++ {
		row := img.Pix[y*img.Stride : y*img.Stride+w*4]
		for x := 0; x < w; x++ {
			if row[x*4+3] >= 8 {
				if x < minX {
					minX = x
				}
				if x > maxX {
					maxX = x
				}
				if y < minY {
					minY = y
				}
				if y > maxY {
					maxY = y
				}
			}
		}
	}
	if maxX < 0 {
		return img, image.Point{}, true
	}
	bw, bh := maxX-minX+1, maxY-minY+1
	dst := image.NewNRGBA(image.Rect(0, 0, bw+2*pad, bh+2*pad))
	for y := 0; y < bh; y++ {
		s := img.PixOffset(minX, minY+y)
		d := dst.PixOffset(pad, pad+y)
		copy(dst.Pix[d:d+bw*4], img.Pix[s:s+bw*4])
	}
	return dst, image.Pt(minX-pad, minY-pad), false
}

// ===========================================================================
// Document cleanup (shadow removal) and redaction for full-page export
// ===========================================================================

// cleanDocument divides by the local paper brightness so shadows vanish and the
// paper turns white while ink stays dark. Returns a new opaque NRGBA.
func cleanDocument(img *image.NRGBA) *image.NRGBA {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	lum := lumPlane(img)
	sat := satTable(lum, w, h)
	sw := w + 1
	rad := bgRadiusFor(w, h)
	out := image.NewNRGBA(img.Rect)
	for y := 0; y < h; y++ {
		y0, y1 := max(0, y-rad), min(h, y+rad+1)
		for x := 0; x < w; x++ {
			x0, x1 := max(0, x-rad), min(w, x+rad+1)
			area := (x1 - x0) * (y1 - y0)
			sum := sat[y1*sw+x1] - sat[y0*sw+x1] - sat[y1*sw+x0] + sat[y0*sw+x0]
			mean := float32(sum) / float32(area)
			if mean < 24 {
				mean = 24
			}
			f := 255 / mean
			i := y*img.Stride + x*4
			var c [3]float32
			for k := 0; k < 3; k++ {
				v := float32(img.Pix[i+k]) * f
				if v > 255 {
					v = 255
				}
				c[k] = v
			}
			if 0.299*c[0]+0.587*c[1]+0.114*c[2] >= 236 {
				c[0], c[1], c[2] = 255, 255, 255
			}
			out.Pix[i], out.Pix[i+1], out.Pix[i+2], out.Pix[i+3] = uint8(c[0]+0.5), uint8(c[1]+0.5), uint8(c[2]+0.5), 255
		}
	}
	return out
}

// ===========================================================================
// Watermark (anti-fraud micro text)
// ===========================================================================

var (
	goFontOnce sync.Once
	goFont     *opentype.Font
	goFontErr  error
)

func wmFace(px float64) (font.Face, error) {
	goFontOnce.Do(func() {
		goFont, goFontErr = opentype.Parse(goregular.TTF)
	})
	if goFontErr != nil {
		return nil, goFontErr
	}
	return opentype.NewFace(goFont, &opentype.FaceOptions{Size: px, DPI: 72, Hinting: font.HintingNone})
}

// renderTextMask draws text into an Alpha mask; returns the mask, its text width and font height.
func renderTextMask(text string, px float64) (*image.Alpha, error) {
	face, err := wmFace(px)
	if err != nil {
		return nil, err
	}
	defer face.Close()
	m := face.Metrics()
	tw := (&font.Drawer{Face: face}).MeasureString(text).Ceil()
	lh := m.Ascent.Ceil() + m.Descent.Ceil() + 2
	mask := image.NewAlpha(image.Rect(0, 0, max(1, tw+2), max(1, lh)))
	d := &font.Drawer{
		Dst:  mask,
		Src:  image.Opaque,
		Face: face,
		Dot:  fixed.P(1, m.Ascent.Ceil()+1),
	}
	d.DrawString(text)
	return mask, nil
}

func watermarkText(t string) string {
	return strings.ReplaceAll(t, "{date}", time.Now().Format("2006-01-02"))
}

func drawWatermark(img *image.NRGBA, wm Watermark) {
	if !wm.Enabled {
		return
	}
	text := watermarkText(strings.TrimSpace(wm.Text))
	if text == "" {
		return
	}
	w, h := img.Rect.Dx(), img.Rect.Dy()
	wr, wg, wb, ok := parseHex(wm.Color)
	if !ok {
		wr, wg, wb = 0, 0, 0
	}
	opacity := float64(clampInt(wm.Opacity, 1, 100)) / 100
	px := math.Max(6, math.Min(400, float64(wm.Size)*float64(max(w, h))/700))

	if wm.Mode == "bottom" {
		mask, err := renderTextMask(text, px)
		if err != nil {
			return
		}
		if mw := mask.Rect.Dx(); mw > w-8 && w > 24 {
			px = math.Max(4, px*float64(w-8)/float64(mw))
			if mask, err = renderTextMask(text, px); err != nil {
				return
			}
		}
		mw, mh := mask.Rect.Dx(), mask.Rect.Dy()
		ox := (w - mw) / 2
		oy := h - mh - max(2, int(px*0.4))
		for y := 0; y < mh; y++ {
			dy := oy + y
			if dy < 0 || dy >= h {
				continue
			}
			for x := 0; x < mw; x++ {
				dx := ox + x
				if dx < 0 || dx >= w {
					continue
				}
				if a := mask.Pix[y*mask.Stride+x]; a > 0 {
					blendPixel(img.Pix[img.PixOffset(dx, dy):], wr, wg, wb, float64(a)/255*opacity)
				}
			}
		}
		return
	}

	// Diagonal: tile the text over the whole image, rising 30 degrees, supersampled 2x.
	const ss = 2
	mask, err := renderTextMask(text, px*ss)
	if err != nil {
		return
	}
	mw, mh := mask.Rect.Dx(), mask.Rect.Dy()
	tw := float64(mw) / ss
	pu := tw + px*3
	pv := math.Max(float64(mh)/ss*2.6, px*3.2)
	ang := 30 * math.Pi / 180
	ca, sa := math.Cos(ang), math.Sin(ang)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			u := float64(x)*ca - float64(y)*sa
			v := float64(x)*sa + float64(y)*ca
			row := math.Floor(v / pv)
			vm := v - row*pv
			if vm*ss >= float64(mh) {
				continue
			}
			uu := u
			if int64(row)%2 != 0 {
				uu += pu / 2
			}
			um := math.Mod(uu, pu)
			if um < 0 {
				um += pu
			}
			mx, my := int(um*ss), int(vm*ss)
			if mx >= mw {
				continue
			}
			var sum, cnt int
			for oy := 0; oy < ss; oy++ {
				for ox := 0; ox < ss; ox++ {
					xx, yy := mx+ox, my+oy
					if xx < mw && yy < mh {
						sum += int(mask.Pix[yy*mask.Stride+xx])
						cnt++
					}
				}
			}
			if cnt == 0 || sum == 0 {
				continue
			}
			blendPixel(img.Pix[img.PixOffset(x, y):], wr, wg, wb, float64(sum)/float64(cnt)/255*opacity)
		}
	}
}

// fitCanvas places img centred inside a cw x ch transparent canvas (legacy "form size").
func fitCanvas(img *image.NRGBA, cw, ch int) *image.NRGBA {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	sc := math.Min(float64(cw)/float64(w), float64(ch)/float64(h))
	nw := min(cw, max(1, int(math.Round(float64(w)*sc))))
	nh := min(ch, max(1, int(math.Round(float64(h)*sc))))
	ox, oy := (cw-nw)/2, (ch-nh)/2
	return scaleInto(img, image.Rect(ox, oy, ox+nw, oy+nh), cw, ch, draw.CatmullRom)
}

// finalizeInk applies output sizing (upscale / form canvas) and the watermark.
// upscale <= 1 is used for previews.
func finalizeInk(layer *image.NRGBA, s Settings, upscale int) *image.NRGBA {
	res := layer
	w, h := res.Rect.Dx(), res.Rect.Dy()
	if s.FormCanvas {
		res = fitCanvas(res, formW, formH)
	} else if upscale > 1 {
		f := float64(upscale)
		f = math.Min(f, float64(maxOutDim)/float64(max(w, h)))
		f = math.Min(f, math.Sqrt(float64(maxOutPixels)/float64(w*h)))
		if fi := int(math.Floor(f + 1e-6)); fi >= 2 {
			res = scaleInto(res, image.Rect(0, 0, w*fi, h*fi), w*fi, h*fi, draw.CatmullRom)
		}
	}
	if s.Watermark.Enabled {
		if res == layer { // never mutate the caller's layer
			c := image.NewNRGBA(res.Rect)
			copy(c.Pix, res.Pix)
			res = c
		}
		drawWatermark(res, s.Watermark)
	}
	return res
}

// ===========================================================================
// Encoders: PNG (smart palette), JPEG, size limiter
// ===========================================================================

func encodePNG(img image.Image, level png.CompressionLevel) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: level}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// uniformInk reports whether every non-transparent pixel has one single RGB colour.
func uniformInk(img *image.NRGBA) (r, g, b uint8, ok bool) {
	found := false
	for i := 0; i+3 < len(img.Pix); i += 4 {
		if img.Pix[i+3] == 0 {
			continue
		}
		if !found {
			r, g, b, found = img.Pix[i], img.Pix[i+1], img.Pix[i+2], true
			continue
		}
		if img.Pix[i] != r || img.Pix[i+1] != g || img.Pix[i+2] != b {
			return 0, 0, 0, false
		}
	}
	return r, g, b, found
}

// encodePNGSmart writes a 1-byte-per-pixel palette PNG (palette index = alpha) when the
// layer is single-coloured, which is several times smaller than 32-bit RGBA.
func encodePNGSmart(img *image.NRGBA, level png.CompressionLevel) ([]byte, error) {
	if r, g, b, ok := uniformInk(img); ok {
		pal := make(color.Palette, 256)
		for i := range pal {
			pal[i] = color.NRGBA{R: r, G: g, B: b, A: uint8(i)}
		}
		p := image.NewPaletted(img.Rect, pal)
		for y := 0; y < img.Rect.Dy(); y++ {
			for x := 0; x < img.Rect.Dx(); x++ {
				p.Pix[y*p.Stride+x] = img.Pix[y*img.Stride+x*4+3]
			}
		}
		return encodePNG(p, level)
	}
	return encodePNG(img, level)
}

func sizeLimitBytes(mode string) int {
	switch mode {
	case "kb500":
		return 480 * 1000
	case "mb1":
		return 970 * 1000
	}
	return 0
}

// encodePNGLimited tunes compression and downsizes until the PNG fits maxBytes.
func encodePNGLimited(img *image.NRGBA, maxBytes int) ([]byte, *image.NRGBA, error) {
	cur := img
	var data []byte
	for iter := 0; iter < 10; iter++ {
		var err error
		data, err = encodePNGSmart(cur, png.BestCompression)
		if err != nil {
			return nil, nil, err
		}
		w, h := cur.Rect.Dx(), cur.Rect.Dy()
		if len(data) <= maxBytes || min(w, h) < 24 {
			break
		}
		f := math.Sqrt(float64(maxBytes)/float64(len(data))) * 0.93
		f = math.Max(0.3, math.Min(0.92, f))
		nw, nh := max(1, int(math.Round(float64(w)*f))), max(1, int(math.Round(float64(h)*f)))
		cur = scaleInto(cur, image.Rect(0, 0, nw, nh), nw, nh, draw.CatmullRom)
	}
	return data, cur, nil
}

// encodeJPEGLimited lowers quality, then size, until the JPEG fits maxBytes.
func encodeJPEGLimited(img *image.NRGBA, maxBytes int) ([]byte, *image.NRGBA, error) {
	cur := flatten(img)
	for _, q := range []int{92, 85, 78, 70, 62, 54, 46} {
		data, err := jpegBytes(cur, q)
		if err != nil {
			return nil, nil, err
		}
		if len(data) <= maxBytes {
			return data, cur, nil
		}
	}
	var data []byte
	for iter := 0; iter < 10; iter++ {
		var err error
		if data, err = jpegBytes(cur, 60); err != nil {
			return nil, nil, err
		}
		w, h := cur.Rect.Dx(), cur.Rect.Dy()
		if len(data) <= maxBytes || min(w, h) < 64 {
			break
		}
		f := math.Max(0.4, math.Min(0.9, math.Sqrt(float64(maxBytes)/float64(len(data)))*0.95))
		nw, nh := max(1, int(math.Round(float64(w)*f))), max(1, int(math.Round(float64(h)*f)))
		cur = scaleInto(cur, image.Rect(0, 0, nw, nh), nw, nh, draw.CatmullRom)
		cur = flatten(cur)
	}
	return data, cur, nil
}

// ===========================================================================
// Vector SVG: marching squares + Douglas-Peucker + quadratic smoothing
// ===========================================================================

type fpt struct{ x, y float64 }

func dpSimplify(p []fpt, eps float64) []fpt {
	n := len(p)
	if n < 3 {
		return append([]fpt(nil), p...)
	}
	keep := make([]bool, n)
	keep[0], keep[n-1] = true, true
	type seg struct{ a, b int }
	stack := []seg{{0, n - 1}}
	for len(stack) > 0 {
		sg := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		a, b := p[sg.a], p[sg.b]
		dx, dy := b.x-a.x, b.y-a.y
		l2 := dx*dx + dy*dy
		best, bi := -1.0, -1
		for i := sg.a + 1; i < sg.b; i++ {
			var d float64
			if l2 == 0 {
				d = math.Hypot(p[i].x-a.x, p[i].y-a.y)
			} else {
				d = math.Abs(dy*(p[i].x-a.x)-dx*(p[i].y-a.y)) / math.Sqrt(l2)
			}
			if d > best {
				best, bi = d, i
			}
		}
		if bi >= 0 && best > eps {
			keep[bi] = true
			stack = append(stack, seg{sg.a, bi}, seg{bi, sg.b})
		}
	}
	out := make([]fpt, 0, n/4+4)
	for i, k := range keep {
		if k {
			out = append(out, p[i])
		}
	}
	return out
}

func simplifyClosed(p []fpt, eps float64) []fpt {
	n := len(p)
	if n < 8 {
		return p
	}
	far, fd := 0, -1.0
	for i := 1; i < n; i++ {
		if d := math.Hypot(p[i].x-p[0].x, p[i].y-p[0].y); d > fd {
			far, fd = i, d
		}
	}
	a := dpSimplify(p[:far+1], eps)
	tail := make([]fpt, 0, n-far+1)
	tail = append(tail, p[far:]...)
	tail = append(tail, p[0])
	b := dpSimplify(tail, eps)
	out := make([]fpt, 0, len(a)+len(b))
	out = append(out, a...)
	if len(b) > 2 {
		out = append(out, b[1:len(b)-1]...)
	}
	return out
}

func polyArea(p []fpt) float64 {
	var s float64
	for i := range p {
		j := (i + 1) % len(p)
		s += p[i].x*p[j].y - p[j].x*p[i].y
	}
	return math.Abs(s) / 2
}

func fmtNum(sb *strings.Builder, v float64) {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	if s == "-0" || s == "" {
		s = "0"
	}
	sb.WriteString(s)
}

// traceMask converts a 0..255 coverage mask into SVG path data (even-odd fill) by
// extracting the 50% iso-contour with sub-pixel marching squares.
func traceMask(mask []uint8, w, h int, eps float64) string {
	const T = 128.0
	W := w + 2
	val := func(x, y int) float64 {
		if x < 0 || y < 0 || x >= w || y >= h {
			return 0
		}
		return float64(mask[y*w+x])
	}
	hKey := func(x, y int) int32 { return int32(((y+1)*W + (x + 1)) * 2) }
	vKey := func(x, y int) int32 { return int32(((y+1)*W+(x+1))*2 + 1) }
	type nb struct {
		a, b int32
		n    uint8
	}
	adj := make(map[int32]nb)
	add := func(p, q int32) {
		e := adj[p]
		switch e.n {
		case 0:
			e.a = q
		case 1:
			e.b = q
		default:
			return
		}
		e.n++
		adj[p] = e
	}
	link := func(p, q int32) { add(p, q); add(q, p) }

	for cy := -1; cy < h; cy++ {
		for cx := -1; cx < w; cx++ {
			vtl, vtr := val(cx, cy), val(cx+1, cy)
			vbl, vbr := val(cx, cy+1), val(cx+1, cy+1)
			idx := 0
			if vtl >= T {
				idx |= 8
			}
			if vtr >= T {
				idx |= 4
			}
			if vbr >= T {
				idx |= 2
			}
			if vbl >= T {
				idx |= 1
			}
			if idx == 0 || idx == 15 {
				continue
			}
			top, bottom := hKey(cx, cy), hKey(cx, cy+1)
			left, right := vKey(cx, cy), vKey(cx+1, cy)
			switch idx {
			case 1, 14:
				link(left, bottom)
			case 2, 13:
				link(bottom, right)
			case 3, 12:
				link(left, right)
			case 4, 11:
				link(top, right)
			case 6, 9:
				link(top, bottom)
			case 7, 8:
				link(top, left)
			case 5:
				if (vtl+vtr+vbr+vbl)/4 >= T {
					link(top, left)
					link(bottom, right)
				} else {
					link(top, right)
					link(left, bottom)
				}
			case 10:
				if (vtl+vtr+vbr+vbl)/4 >= T {
					link(top, right)
					link(left, bottom)
				} else {
					link(top, left)
					link(bottom, right)
				}
			}
		}
	}
	pos := func(k int32) fpt {
		idx := int(k >> 1)
		x, y := idx%W-1, idx/W-1
		if k&1 == 0 {
			a, b := val(x, y), val(x+1, y)
			t := 0.5
			if b != a {
				t = (T - a) / (b - a)
			}
			return fpt{float64(x) + t + 0.5, float64(y) + 0.5}
		}
		a, b := val(x, y), val(x, y+1)
		t := 0.5
		if b != a {
			t = (T - a) / (b - a)
		}
		return fpt{float64(x) + 0.5, float64(y) + t + 0.5}
	}

	keys := make([]int32, 0, len(adj))
	for k := range adj {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	visited := make(map[int32]struct{}, len(adj))
	var sb strings.Builder
	for _, k0 := range keys {
		if _, seen := visited[k0]; seen {
			continue
		}
		var loop []fpt
		prev, cur := int32(-1), k0
		for {
			visited[cur] = struct{}{}
			loop = append(loop, pos(cur))
			e := adj[cur]
			next := int32(-1)
			if e.n >= 1 && e.a != prev {
				if _, s := visited[e.a]; !s {
					next = e.a
				}
			}
			if next < 0 && e.n >= 2 && e.b != prev {
				if _, s := visited[e.b]; !s {
					next = e.b
				}
			}
			if next < 0 {
				break
			}
			prev, cur = cur, next
		}
		if len(loop) < 4 {
			continue
		}
		poly := simplifyClosed(loop, eps)
		if len(poly) < 3 || polyArea(poly) < 3 {
			continue
		}
		n := len(poly)
		mid := func(a, b fpt) fpt { return fpt{(a.x + b.x) / 2, (a.y + b.y) / 2} }
		m0 := mid(poly[n-1], poly[0])
		sb.WriteString("M")
		fmtNum(&sb, m0.x)
		sb.WriteByte(' ')
		fmtNum(&sb, m0.y)
		for i := 0; i < n; i++ {
			c := poly[i]
			m := mid(poly[i], poly[(i+1)%n])
			sb.WriteString("Q")
			fmtNum(&sb, c.x)
			sb.WriteByte(' ')
			fmtNum(&sb, c.y)
			sb.WriteByte(' ')
			fmtNum(&sb, m.x)
			sb.WriteByte(' ')
			fmtNum(&sb, m.y)
		}
		sb.WriteString("Z")
	}
	return sb.String()
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;")
	return r.Replace(s)
}

// buildSVG vectorises the (trimmed) ink layer. Stamp-coloured and pen-coloured ink are
// written as two separate paths so multi-colour originals keep their colours.
func buildSVG(layer *image.NRGBA, upscale int, wm Watermark) []byte {
	w, h := layer.Rect.Dx(), layer.Rect.Dy()
	up := clampInt(upscale, 1, 3)
	stampMask := make([]uint8, w*h)
	sigMask := make([]uint8, w*h)
	var sr, sg, sb, sw, gr, gg, gb, gw float64
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*layer.Stride + x*4
			a := layer.Pix[i+3]
			if a == 0 {
				continue
			}
			_, st := inkClassWeights(layer.Pix[i], layer.Pix[i+1], layer.Pix[i+2])
			sa := float32(a) * st
			ga := float32(a) - sa
			stampMask[y*w+x] = uint8(sa + 0.5)
			sigMask[y*w+x] = uint8(ga + 0.5)
			sr += float64(layer.Pix[i]) * float64(sa)
			sg += float64(layer.Pix[i+1]) * float64(sa)
			sb += float64(layer.Pix[i+2]) * float64(sa)
			sw += float64(sa)
			gr += float64(layer.Pix[i]) * float64(ga)
			gg += float64(layer.Pix[i+1]) * float64(ga)
			gb += float64(layer.Pix[i+2]) * float64(ga)
			gw += float64(ga)
		}
	}
	eps := 0.35
	var out strings.Builder
	out.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	fmt.Fprintf(&out, "<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"%d\" height=\"%d\" viewBox=\"0 0 %d %d\">\n", w*up, h*up, w, h)
	out.WriteString("<title>Photo BG vector export</title>\n")
	emit := func(mask []uint8, r, g, b, wsum float64) {
		if wsum <= 0 {
			return
		}
		d := traceMask(mask, w, h, eps)
		if d == "" {
			return
		}
		fmt.Fprintf(&out, "<path fill=\"#%02X%02X%02X\" fill-rule=\"evenodd\" d=\"%s\"/>\n",
			uint8(r/wsum+0.5), uint8(g/wsum+0.5), uint8(b/wsum+0.5), d)
	}
	emit(sigMask, gr, gg, gb, gw)
	emit(stampMask, sr, sg, sb, sw)

	if wm.Enabled {
		if text := watermarkText(strings.TrimSpace(wm.Text)); text != "" {
			wr, wg, wb, ok := parseHex(wm.Color)
			if !ok {
				wr, wg, wb = 0, 0, 0
			}
			fs := math.Max(6, float64(wm.Size)*float64(max(w, h))/700)
			col := fmt.Sprintf("#%02X%02X%02X", wr, wg, wb)
			op := float64(clampInt(wm.Opacity, 1, 100)) / 100
			et := xmlEscape(text)
			if wm.Mode == "bottom" {
				fmt.Fprintf(&out, "<text x=\"%.2f\" y=\"%.2f\" text-anchor=\"middle\" font-family=\"Arial, Helvetica, sans-serif\" font-size=\"%.2f\" fill=\"%s\" fill-opacity=\"%.2f\">%s</text>\n",
					float64(w)/2, float64(h)-fs*0.5, fs, col, op, et)
			} else {
				pu := float64(len([]rune(text)))*fs*0.55 + fs*3
				pv := fs * 3.2
				fmt.Fprintf(&out, "<defs><pattern id=\"wm\" width=\"%.2f\" height=\"%.2f\" patternUnits=\"userSpaceOnUse\" patternTransform=\"rotate(-30)\"><text x=\"0\" y=\"%.2f\" font-family=\"Arial, Helvetica, sans-serif\" font-size=\"%.2f\" fill=\"%s\" fill-opacity=\"%.2f\">%s</text></pattern></defs>\n",
					pu, pv, fs, fs, col, op, et)
				fmt.Fprintf(&out, "<rect width=\"%d\" height=\"%d\" fill=\"url(#wm)\"/>\n", w, h)
			}
		}
	}
	out.WriteString("</svg>\n")
	return []byte(out.String())
}

// ===========================================================================
// Pure-Go PDF writer (image page, optional soft mask)
// ===========================================================================

func pdfNum(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

// buildPDF writes a one-page PDF. When jpegData is non-nil it is embedded as-is
// (DCTDecode); otherwise the image is stored as Flate RGB with an alpha soft mask.
// mode "a4" fits the image on an A4 page inside margin points; "auto" sizes the page
// to the image at 150 DPI.
func buildPDF(img *image.NRGBA, jpegData []byte, mode string, margin float64) ([]byte, error) {
	iw, ih := img.Rect.Dx(), img.Rect.Dy()
	if iw < 1 || ih < 1 {
		return nil, errors.New("empty image")
	}
	hasAlpha := false
	if jpegData == nil {
		for i := 3; i < len(img.Pix); i += 4 {
			if img.Pix[i] != 255 {
				hasAlpha = true
				break
			}
		}
	}
	const natural = 72.0 / 150.0 // points per pixel at 150 DPI
	var pw, ph, dw, dh float64
	if mode == "auto" {
		dw, dh = float64(iw)*natural, float64(ih)*natural
		if m := math.Max(dw, dh); m > 14400 {
			dw, dh = dw*14400/m, dh*14400/m
		}
		pw, ph = dw, dh
	} else {
		pw, ph = 595.28, 841.89
		if !hasAlpha && jpegData != nil && iw > ih {
			pw, ph = ph, pw
		}
		cw, ch := pw-2*margin, ph-2*margin
		sc := math.Min(cw/float64(iw), ch/float64(ih))
		if hasAlpha {
			sc = math.Min(sc, natural)
		}
		dw, dh = float64(iw)*sc, float64(ih)*sc
	}
	ox, oy := (pw-dw)/2, (ph-dh)/2

	var buf bytes.Buffer
	offs := make([]int, 0, 8)
	begin := func() { offs = append(offs, buf.Len()) }
	buf.WriteString("%PDF-1.4\n%\xE2\xE3\xCF\xD3\n")

	begin()
	buf.WriteString("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	begin()
	buf.WriteString("2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")
	begin()
	fmt.Fprintf(&buf, "3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %s %s] /Resources << /XObject << /Im0 5 0 R >> >> /Contents 4 0 R >>\nendobj\n",
		pdfNum(pw), pdfNum(ph))
	content := fmt.Sprintf("q\n%s 0 0 %s %s %s cm\n/Im0 Do\nQ\n", pdfNum(dw), pdfNum(dh), pdfNum(ox), pdfNum(oy))
	begin()
	fmt.Fprintf(&buf, "4 0 obj\n<< /Length %d >>\nstream\n%sendstream\nendobj\n", len(content), content)

	begin()
	if jpegData != nil {
		fmt.Fprintf(&buf, "5 0 obj\n<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /DCTDecode /Length %d >>\nstream\n",
			iw, ih, len(jpegData))
		buf.Write(jpegData)
		buf.WriteString("\nendstream\nendobj\n")
	} else {
		var rgb bytes.Buffer
		zw := zlib.NewWriter(&rgb)
		row := make([]byte, iw*3)
		for y := 0; y < ih; y++ {
			p := img.Pix[y*img.Stride : y*img.Stride+iw*4]
			for x := 0; x < iw; x++ {
				row[x*3], row[x*3+1], row[x*3+2] = p[x*4], p[x*4+1], p[x*4+2]
			}
			if _, err := zw.Write(row); err != nil {
				return nil, err
			}
		}
		if err := zw.Close(); err != nil {
			return nil, err
		}
		smask := ""
		if hasAlpha {
			smask = " /SMask 6 0 R"
		}
		fmt.Fprintf(&buf, "5 0 obj\n<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /FlateDecode%s /Length %d >>\nstream\n",
			iw, ih, smask, rgb.Len())
		buf.Write(rgb.Bytes())
		buf.WriteString("\nendstream\nendobj\n")
		if hasAlpha {
			var ab bytes.Buffer
			zw := zlib.NewWriter(&ab)
			arow := make([]byte, iw)
			for y := 0; y < ih; y++ {
				p := img.Pix[y*img.Stride : y*img.Stride+iw*4]
				for x := 0; x < iw; x++ {
					arow[x] = p[x*4+3]
				}
				if _, err := zw.Write(arow); err != nil {
					return nil, err
				}
			}
			if err := zw.Close(); err != nil {
				return nil, err
			}
			begin()
			fmt.Fprintf(&buf, "6 0 obj\n<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceGray /BitsPerComponent 8 /Filter /FlateDecode /Length %d >>\nstream\n",
				iw, ih, ab.Len())
			buf.Write(ab.Bytes())
			buf.WriteString("\nendstream\nendobj\n")
		}
	}
	begin()
	fmt.Fprintf(&buf, "%d 0 obj\n<< /Producer (Photo BG %s) /Creator (Photo BG) >>\nendobj\n", len(offs), appVersion)
	infoObj := len(offs)

	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(offs)+1)
	for _, o := range offs {
		fmt.Fprintf(&buf, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R /Info %d 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offs)+1, infoObj, xref)
	return buf.Bytes(), nil
}

// ===========================================================================
// Output rendering + atomic writing
// ===========================================================================

type outFile struct {
	tag  string
	ext  string
	data []byte
	w, h int
}

func exportCap(s Settings, kind string) int {
	if kind == "ink" && s.Upscale > 1 {
		return exportCapHD
	}
	return exportCapSmall
}

// renderOutputs runs the full-resolution pipeline for one file and returns encoded outputs.
func renderOutputs(src string, s Settings, o ExportOptions, bud *pixelBudget) (outs []outFile, err error) {
	defer func() {
		if r := recover(); r != nil {
			outs, err = nil, fmt.Errorf("Unexpected failure: %v", r)
		}
	}()
	s = normalizeSettings(s)
	o = normalizeOptions(o)

	w, h, err := imageDims(src)
	if err != nil {
		return nil, err
	}
	px := int64(w) * int64(h)
	bud.acquire(px)
	released := false
	release := func() {
		if !released {
			released = true
			bud.release(px)
		}
	}
	defer release()
	if px > 20_000_000 {
		defer debug.FreeOSMemory()
	}

	img, err := decodeFile(src)
	if err != nil {
		return nil, err
	}
	orients := orientsFor(exifOrientation(src), s)
	frame, _, err := buildFrame(img, orients, s, exportCap(s, o.Kind))
	img = nil
	release()
	if err != nil {
		return nil, err
	}

	limit := sizeLimitBytes(s.SizeMode)

	if o.Kind == "document" {
		doc := frame
		if o.CleanDoc {
			doc = cleanDocument(frame)
		} else {
			doc = image.NewNRGBA(frame.Rect)
			copy(doc.Pix, frame.Pix)
		}
		applyRedactions(doc, s.Strokes)
		drawWatermark(doc, s.Watermark)

		var jpgData []byte
		finalImg := doc
		if limit > 0 {
			jpgData, finalImg, err = encodeJPEGLimited(doc, limit)
		} else {
			jpgData, err = jpegBytes(doc, 92)
		}
		if err != nil {
			return nil, err
		}
		fw, fh := finalImg.Rect.Dx(), finalImg.Rect.Dy()
		for _, f := range o.Formats {
			switch f {
			case "jpg":
				outs = append(outs, outFile{"_clean", "jpg", jpgData, fw, fh})
			case "png":
				if limit > 0 {
					continue // size-limited documents are written as JPEG instead
				}
				d, e := encodePNG(doc, png.DefaultCompression)
				if e != nil {
					return nil, e
				}
				outs = append(outs, outFile{"_clean", "png", d, fw, fh})
			case "pdf":
				pdfJPEG := jpgData
				if limit == 0 {
					if pdfJPEG, err = jpegBytes(doc, 90); err != nil {
						return nil, err
					}
				}
				d, e := buildPDF(finalImg, pdfJPEG, o.PDFMode, 18)
				if e != nil {
					return nil, e
				}
				outs = append(outs, outFile{"_clean", "pdf", d, fw, fh})
			}
		}
		if len(outs) == 0 { // e.g. PNG requested with a size limit
			outs = append(outs, outFile{"_clean", "jpg", jpgData, fw, fh})
		}
		return outs, nil
	}

	type variant struct{ sep, tag string }
	variants := []variant{{s.Separator, "_PhotoBG"}}
	if o.Split {
		variants = []variant{{"signature", "_signature"}, {"stamp", "_stamp"}}
	}
	anyInk := false
	for _, v := range variants {
		vs := s
		vs.Separator = v.sep
		layer, _, empty := renderInk(frame, vs, 1)
		if empty {
			continue
		}
		anyInk = true
		final := finalizeInk(layer, vs, vs.Upscale)
		for _, f := range o.Formats {
			switch f {
			case "png":
				var d []byte
				var e error
				if limit > 0 {
					d, final, e = encodePNGLimited(final, limit)
				} else {
					d, e = encodePNG(final, png.DefaultCompression)
				}
				if e != nil {
					return nil, e
				}
				outs = append(outs, outFile{v.tag, "png", d, final.Rect.Dx(), final.Rect.Dy()})
			case "svg":
				up := vs.Upscale
				if vs.FormCanvas {
					up = 1
				}
				outs = append(outs, outFile{v.tag, "svg", buildSVG(layer, up, vs.Watermark), layer.Rect.Dx() * up, layer.Rect.Dy() * up})
			case "pdf":
				d, e := buildPDF(final, nil, o.PDFMode, 36)
				if e != nil {
					return nil, e
				}
				outs = append(outs, outFile{v.tag, "pdf", d, final.Rect.Dx(), final.Rect.Dy()})
			case "jpg":
				// JPEG has no alpha: flatten onto white
				d, e := jpegBytes(flatten(final), 92)
				if e != nil {
					return nil, e
				}
				outs = append(outs, outFile{v.tag, "jpg", d, final.Rect.Dx(), final.Rect.Dy()})
			}
		}
	}
	if !anyInk {
		if o.Split {
			return nil, errors.New("No signature or stamp ink found")
		}
		return nil, errors.New("Nothing left after processing. Lower Transparency or check the settings")
	}
	return outs, nil
}

// writeUnique claims <dir>/<base>[_N].<ext> atomically with O_EXCL and writes data.
func writeUnique(dir, base, ext string, data []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	for i := 0; i < 10000; i++ {
		name := base + "." + ext
		if i > 0 {
			name = fmt.Sprintf("%s_%d.%s", base, i, ext)
		}
		p := filepath.Join(dir, name)
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return "", err
		}
		_, werr := f.Write(data)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			_ = os.Remove(p)
			return "", werr
		}
		return p, nil
	}
	return "", errors.New("Could not find a free file name")
}

// exportFile renders and writes all requested outputs for one source image.
func exportFile(src string, s Settings, o ExportOptions, bud *pixelBudget) (files []string, total int64, err error) {
	o = normalizeOptions(o)
	outs, err := renderOutputs(src, s, o, bud)
	if err != nil {
		return nil, 0, err
	}
	outDir := resolveOutDir(src, o.OutDir)
	stem := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
	for _, of := range outs {
		var p string
		if o.OutName != "" {
			name := o.OutName
			if of.tag == "_signature" || of.tag == "_stamp" {
				name += of.tag
			}
			if err := os.MkdirAll(outDir, 0o755); err != nil {
				return files, total, err
			}
			p = filepath.Join(outDir, name+"."+of.ext)
			if err := writeFileAtomic(p, of.data); err != nil {
				return files, total, err
			}
		} else {
			if p, err = writeUnique(outDir, stem+of.tag, of.ext, of.data); err != nil {
				return files, total, err
			}
		}
		files = append(files, p)
		total += int64(len(of.data))
	}
	return files, total, nil
}

// ===========================================================================
// Thumbnails, preview, studio image
// ===========================================================================

func (a *App) GetThumb(filePath string) (url string) {
	defer func() {
		if r := recover(); r != nil {
			url = ""
		}
	}()
	filePath = normalizePath(filePath)
	if !isImageExt(filePath) {
		return ""
	}
	img, err := decodeFile(filePath)
	if err != nil {
		return ""
	}
	thumb, err := orientCrop(img, orientsFor(exifOrientation(filePath), Settings{}), nil, thumbMax)
	if err != nil {
		return ""
	}
	out, err := pngDataURL(thumb)
	if err != nil {
		return ""
	}
	return out
}

// previewSource returns the cached, flattened <=previewMax version of a photo.
func (a *App) previewSource(path string) (*previewSrc, string, error) {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return nil, "", errors.New("Photo not found")
	}
	key := fmt.Sprintf("%s|%d|%d", path, fi.Size(), fi.ModTime().UnixNano())

	a.mu.Lock()
	if ps, ok := a.cache[key]; ok {
		a.mu.Unlock()
		return ps, key, nil
	}
	a.mu.Unlock()

	img, err := decodeFile(path)
	if err != nil {
		return nil, "", err
	}
	nw, nh := img.Bounds().Dx(), img.Bounds().Dy()
	base, err := orientCrop(img, nil, nil, previewMax)
	if err != nil {
		return nil, "", err
	}
	ps := &previewSrc{base: base, nativeW: nw, nativeH: nh, exif: exifOrientation(path)}

	a.mu.Lock()
	if _, ok := a.cache[key]; !ok {
		a.cache[key] = ps
		a.order = append(a.order, key)
		for len(a.order) > previewCacheMax {
			delete(a.cache, a.order[0])
			a.order = a.order[1:]
		}
	}
	a.mu.Unlock()
	return ps, key, nil
}

func frameKeyFor(srcKey string, orients []orient, s Settings) string {
	var sb strings.Builder
	sb.WriteString(srcKey)
	for _, o := range orients {
		fmt.Fprintf(&sb, "|o%d%v%v", o.rot, o.fh, o.fv)
	}
	if s.Crop != nil {
		fmt.Fprintf(&sb, "|c%.5f,%.5f,%.5f,%.5f", s.Crop.X, s.Crop.Y, s.Crop.W, s.Crop.H)
	}
	fmt.Fprintf(&sb, "|d%v", s.Deskew)
	return sb.String()
}

// frameFor returns the (cached) oriented/cropped/deskewed preview frame.
func (a *App) frameFor(ps *previewSrc, srcKey string, orients []orient, s Settings) (*frameEntry, error) {
	key := frameKeyFor(srcKey, orients, s)
	a.mu.Lock()
	if a.frameC != nil && a.frameC.key == key {
		fe := a.frameC
		a.mu.Unlock()
		return fe, nil
	}
	a.mu.Unlock()

	frame, angle, err := buildFrame(ps.base, orients, s, previewMax)
	if err != nil {
		return nil, err
	}
	orig, err := jpegDataURL(frame, 90)
	if err != nil {
		return nil, err
	}
	fe := &frameEntry{key: key, frame: frame, angle: angle, origURL: orig}
	a.mu.Lock()
	a.frameC = fe
	a.mu.Unlock()
	return fe, nil
}

func estimateOut(w, h int, padScale float64, s Settings) (int, int) {
	if s.FormCanvas {
		return formW, formH
	}
	fw, fh := float64(w)/padScale, float64(h)/padScale
	f := float64(s.Upscale)
	if f < 1 {
		f = 1
	}
	f = math.Min(f, float64(maxOutDim)/math.Max(fw, fh))
	f = math.Min(f, math.Sqrt(float64(maxOutPixels)/(fw*fh)))
	if fi := math.Floor(f + 1e-6); fi >= 2 {
		f = fi
	} else {
		f = 1
	}
	return int(math.Round(fw * f)), int(math.Round(fh * f))
}

// GetPreview renders the fast live preview. knownKey is the FrameKey the frontend already
// holds; when it matches, the (large) Orig data URL is not sent again.
func (a *App) GetPreview(filePath string, s Settings, knownKey string) (res PreviewResult) {
	defer func() {
		if r := recover(); r != nil {
			res = PreviewResult{Error: fmt.Sprintf("Preview failed: %v", r)}
		}
	}()
	t0 := time.Now()
	s = normalizeSettings(s)
	filePath = normalizePath(filePath)
	if !isImageExt(filePath) {
		return PreviewResult{Error: "Not a supported image file"}
	}
	ps, srcKey, err := a.previewSource(filePath)
	if err != nil {
		return PreviewResult{Error: err.Error()}
	}
	orients := orientsFor(ps.exif, s)
	fe, err := a.frameFor(ps, srcKey, orients, s)
	if err != nil {
		return PreviewResult{Error: err.Error()}
	}
	frame := fe.frame
	fw, fh := frame.Rect.Dx(), frame.Rect.Dy()

	ow, oh := orientedDims(ps.nativeW, ps.nativeH, orients)
	cw, ch := float64(ow), float64(oh)
	if s.Crop != nil {
		cw, ch = cw*s.Crop.W, ch*s.Crop.H
	}
	nativeLong := math.Max(1, math.Max(cw, ch))
	exportLong := math.Max(1, math.Min(nativeLong, float64(exportCap(s, "ink"))))
	padScale := math.Max(0.05, math.Min(4, float64(max(fw, fh))/exportLong))

	layer, off, empty := renderInk(frame, s, padScale)
	final := finalizeInk(layer, s, 1)
	if s.FormCanvas {
		off = image.Point{}
	}
	clean, err := pngDataURL(final)
	if err != nil {
		return PreviewResult{Error: err.Error()}
	}
	outW, outH := estimateOut(layer.Rect.Dx(), layer.Rect.Dy(), padScale, s)
	res = PreviewResult{
		Clean:    clean,
		FrameKey: fe.key,
		W:        final.Rect.Dx(),
		H:        final.Rect.Dy(),
		OrigW:    fw,
		OrigH:    fh,
		OffX:     off.X,
		OffY:     off.Y,
		OutW:     outW,
		OutH:     outH,
		Angle:    fe.angle,
		Empty:    empty,
	}
	if knownKey != fe.key {
		res.Orig = fe.origURL
	}
	res.Ms = time.Since(t0).Milliseconds()
	return res
}

// GetStudioImage returns the rotated/flipped (but uncropped) photo for the crop studio.
func (a *App) GetStudioImage(filePath string, rotate int, flipH, flipV bool) (res StudioImage) {
	defer func() {
		if r := recover(); r != nil {
			res = StudioImage{Error: fmt.Sprintf("Studio failed: %v", r)}
		}
	}()
	filePath = normalizePath(filePath)
	if !isImageExt(filePath) {
		return StudioImage{Error: "Not a supported image file"}
	}
	ps, _, err := a.previewSource(filePath)
	if err != nil {
		return StudioImage{Error: err.Error()}
	}
	s := normalizeSettings(Settings{Rotate: rotate, FlipH: flipH, FlipV: flipV})
	orients := orientsFor(ps.exif, s)
	img, err := orientCrop(ps.base, orients, nil, previewMax)
	if err != nil {
		return StudioImage{Error: err.Error()}
	}
	url, err := jpegDataURL(img, 90)
	if err != nil {
		return StudioImage{Error: err.Error()}
	}
	nw, nh := orientedDims(ps.nativeW, ps.nativeH, orients)
	return StudioImage{Img: url, W: img.Rect.Dx(), H: img.Rect.Dy(), NativeW: nw, NativeH: nh}
}

// ===========================================================================
// Export, clipboard, drag-out, composed page, paste-in
// ===========================================================================

// Export renders the current photo with all settings and writes the requested formats.
func (a *App) Export(filePath string, s Settings, o ExportOptions) (res ExportResult) {
	defer func() {
		if r := recover(); r != nil {
			res = ExportResult{Error: fmt.Sprintf("%v", r)}
		}
	}()
	filePath = normalizePath(filePath)
	if !isImageExt(filePath) || !fileExists(filePath) {
		return ExportResult{Error: "Photo not found"}
	}
	o = normalizeOptions(o)
	files, n, err := exportFile(filePath, s, o, nil)
	if err != nil {
		return ExportResult{Error: err.Error()}
	}
	dir := filepath.Dir(files[0])
	if o.OpenWhenDone {
		_ = openDirectory(dir)
	}
	msg := fmt.Sprintf("Saved %d file(s) to %s", len(files), dir)
	if len(files) == 1 {
		msg = "Saved " + filepath.Base(files[0])
	}
	return ExportResult{OK: true, Files: files, OutDir: dir, Bytes: n, Message: msg}
}

// GetPNGBase64 renders the transparent PNG for Ctrl+C (the frontend writes it to the
// system clipboard with the async Clipboard API) and for in-app use.
func (a *App) GetPNGBase64(filePath string, s Settings) (res BlobResult) {
	defer func() {
		if r := recover(); r != nil {
			res = BlobResult{Error: fmt.Sprintf("%v", r)}
		}
	}()
	filePath = normalizePath(filePath)
	if !isImageExt(filePath) || !fileExists(filePath) {
		return BlobResult{Error: "Photo not found"}
	}
	outs, err := renderOutputs(filePath, s, ExportOptions{Kind: "ink", Formats: []string{"png"}}, nil)
	if err != nil {
		return BlobResult{Error: err.Error()}
	}
	stem := strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath))
	return BlobResult{B64: base64.StdEncoding.EncodeToString(outs[0].data), W: outs[0].w, H: outs[0].h, Name: stem + "_PhotoBG.png"}
}

// PrepareDrag writes the processed PNG to a temp file served at /photobg-drag/<name>
// so the frontend can start a native drag-out (DownloadURL) to Explorer / Finder / browsers.
func (a *App) PrepareDrag(filePath string, s Settings) (res DragResult) {
	defer func() {
		if r := recover(); r != nil {
			res = DragResult{Error: fmt.Sprintf("%v", r)}
		}
	}()
	filePath = normalizePath(filePath)
	if !isImageExt(filePath) || !fileExists(filePath) {
		return DragResult{Error: "Photo not found"}
	}
	outs, err := renderOutputs(filePath, s, ExportOptions{Kind: "ink", Formats: []string{"png"}}, nil)
	if err != nil {
		return DragResult{Error: err.Error()}
	}
	stem := strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath))
	stem = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || r == '"' || r < 32 {
			return '_'
		}
		return r
	}, stem)
	dir := appTempDir("drag")
	p, err := writeUnique(dir, stem+"_PhotoBG", "png", outs[0].data)
	if err != nil {
		return DragResult{Error: err.Error()}
	}
	name := filepath.Base(p)
	return DragResult{URL: "/photobg-drag/" + name, File: p, Name: name}
}

// ImportImageData stores pasted / dropped image bytes (base64 or data URL) as a PNG in the
// app temp folder and returns its path (or "Error: ...").
func (a *App) ImportImageData(data string) string {
	defer func() { _ = recover() }()
	if i := strings.Index(data, ","); i >= 0 && strings.HasPrefix(data, "data:") {
		data = data[i+1:]
	}
	if len(data) == 0 || len(data) > maxPasteBytes*4/3 {
		return "Error: Clipboard image is empty or too large"
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(data))
	if err != nil {
		return "Error: Invalid image data"
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width < 1 || cfg.Height < 1 || int64(cfg.Width)*int64(cfg.Height) > maxDecodePixels {
		return "Error: Unsupported image on the clipboard"
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return "Error: Could not decode the clipboard image"
	}
	var out []byte
	if o, ok := img.(*image.NRGBA); ok {
		out, err = encodePNG(o, png.BestSpeed)
	} else {
		out, err = encodePNG(img, png.BestSpeed)
	}
	if err != nil {
		return "Error: " + err.Error()
	}
	name := "pasted_" + time.Now().Format("20060102_150405")
	p, err := writeUnique(appTempDir("paste"), name, "png", out)
	if err != nil {
		return "Error: " + err.Error()
	}
	return p
}

// SaveComposedPage saves the "Test on Paper" simulator canvas (PNG data URL) as PNG / JPG / PDF.
func (a *App) SaveComposedPage(dataURL, format, outDir string) (res ExportResult) {
	defer func() {
		if r := recover(); r != nil {
			res = ExportResult{Error: fmt.Sprintf("%v", r)}
		}
	}()
	b64 := dataURL
	if i := strings.Index(b64, ","); i >= 0 && strings.HasPrefix(b64, "data:") {
		b64 = b64[i+1:]
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil || len(raw) == 0 {
		return ExportResult{Error: "Invalid page image"}
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return ExportResult{Error: "Could not decode the page image"}
	}
	if int64(img.Bounds().Dx())*int64(img.Bounds().Dy()) > maxOutPixels*2 {
		return ExportResult{Error: "Page image is too large"}
	}
	page := flatten(img)
	var data []byte
	ext := strings.ToLower(strings.TrimPrefix(format, "."))
	switch ext {
	case "pdf":
		jb, e := jpegBytes(page, 93)
		if e != nil {
			return ExportResult{Error: e.Error()}
		}
		data, err = buildPDF(page, jb, "a4", 0)
	case "jpg", "jpeg":
		ext = "jpg"
		data, err = jpegBytes(page, 94)
	default:
		ext = "png"
		data, err = encodePNG(page, png.DefaultCompression)
	}
	if err != nil {
		return ExportResult{Error: err.Error()}
	}
	dir := normalizePath(outDir)
	if dir == "" {
		dir = picturesOutDir()
	}
	p, err := writeUnique(dir, "TestOnPaper_"+time.Now().Format("20060102_150405"), ext, data)
	if err != nil {
		return ExportResult{Error: err.Error()}
	}
	return ExportResult{OK: true, Files: []string{p}, OutDir: dir, Bytes: int64(len(data)), Message: "Saved " + filepath.Base(p)}
}

func picturesOutDir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, "Pictures", outDirName)
	}
	return filepath.Join(os.TempDir(), outDirName)
}

// ===========================================================================
// Batch processing (recursive, parallel, cancellable)
// ===========================================================================

func (a *App) emit(name string, data ...interface{}) {
	if a.ctx == nil {
		return
	}
	defer func() { _ = recover() }()
	wailsrt.EventsEmit(a.ctx, name, data...)
}

// CancelBatch stops a running ProcessBatch after the files currently in flight.
func (a *App) CancelBatch() {
	a.batchMu.Lock()
	if a.batchCancel != nil {
		a.batchCancel()
	}
	a.batchMu.Unlock()
}

// ProcessBatch exports every queued photo (files and folders, recursively) with a bounded
// worker pool, emitting "batch-progress" events, and returns a summary report.
func (a *App) ProcessBatch(filePaths []string, s Settings, o ExportOptions) (sum BatchSummary) {
	defer func() {
		if r := recover(); r != nil {
			sum.Message = fmt.Sprintf("Error: %v", r)
		}
	}()
	a.batchMu.Lock()
	if a.batchRunning {
		a.batchMu.Unlock()
		return BatchSummary{Message: "Error: A batch is already running"}
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.batchRunning = true
	a.batchCancel = cancel
	a.batchMu.Unlock()
	defer func() {
		cancel()
		a.batchMu.Lock()
		a.batchRunning = false
		a.batchCancel = nil
		a.batchMu.Unlock()
		debug.FreeOSMemory()
	}()

	files := collectImages(filePaths, maxBatchFiles)
	if len(files) == 0 {
		return BatchSummary{Message: "Error: No valid photos in the queue"}
	}
	s = stripImageSpecific(normalizeSettings(s))
	o = normalizeOptions(o)
	o.OutName = ""
	o.OutDir = resolveOutDir(files[0], o.OutDir)
	if err := os.MkdirAll(o.OutDir, 0o755); err != nil {
		return BatchSummary{Message: "Error: " + err.Error()}
	}

	start := time.Now()
	sum = BatchSummary{Total: len(files), OutDir: o.OutDir}
	bud := newPixelBudget(decodeBudgetPx)
	var done, okN, failN int32
	var repMu sync.Mutex

	jobs := make(chan string)
	var wg sync.WaitGroup
	workers := min(maxWorkers, max(1, runtime.NumCPU()), len(files))
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				if ctx.Err() != nil {
					continue
				}
				outs, _, err := exportFile(p, s, o, bud)
				repMu.Lock()
				if err != nil {
					atomic.AddInt32(&failN, 1)
					if len(sum.Errors) < 50 {
						sum.Errors = append(sum.Errors, filepath.Base(p)+": "+err.Error())
					}
				} else {
					atomic.AddInt32(&okN, 1)
					if len(sum.Files) < 2000 {
						sum.Files = append(sum.Files, outs...)
					}
				}
				repMu.Unlock()
				d := atomic.AddInt32(&done, 1)
				a.emit("batch-progress", map[string]interface{}{
					"done": int(d), "total": len(files), "ok": int(atomic.LoadInt32(&okN)),
					"failed": int(atomic.LoadInt32(&failN)), "current": filepath.Base(p),
				})
			}
		}()
	}
feed:
	for _, p := range files {
		select {
		case <-ctx.Done():
			break feed
		case jobs <- p:
		}
	}
	close(jobs)
	wg.Wait()

	sum.OK, sum.Failed = int(okN), int(failN)
	sum.Cancelled = ctx.Err() != nil
	sum.ElapsedMs = time.Since(start).Milliseconds()
	switch {
	case sum.Cancelled:
		sum.Message = fmt.Sprintf("Cancelled: %d of %d processed", sum.OK, sum.Total)
	case sum.OK == 0:
		first := ""
		if len(sum.Errors) > 0 {
			first = " (" + sum.Errors[0] + ")"
		}
		sum.Message = "Error: Could not process any photos" + first
	case sum.Failed > 0:
		sum.Message = fmt.Sprintf("Processed %d of %d photos, %d failed", sum.OK, sum.Total, sum.Failed)
	default:
		sum.Message = fmt.Sprintf("Processed %d photos to %s", sum.OK, filepath.Base(o.OutDir))
	}
	if sum.OK > 0 && o.OpenWhenDone {
		_ = openDirectory(o.OutDir)
	}
	return sum
}

// ===========================================================================
// Watched-folder Auto-Pilot
// ===========================================================================

type fileSig struct {
	size int64
	mod  int64
}

func sigOf(p string) (fileSig, bool) {
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return fileSig{}, false
	}
	return fileSig{fi.Size(), fi.ModTime().UnixNano()}, true
}

func (a *App) watchStatusLocked() WatchStatus {
	st := WatchStatus{Processed: a.wProc, Failed: a.wFail, Last: a.wLast, Error: a.wErr}
	if a.w != nil {
		st.Running, st.Inbox, st.Outbox = true, a.w.cfg.Inbox, a.w.cfg.Outbox
	}
	return st
}

func (a *App) WatchStatus() WatchStatus {
	a.wMu.Lock()
	defer a.wMu.Unlock()
	return a.watchStatusLocked()
}

// UpdateWatchSettings makes the running watcher use new settings / options.
func (a *App) UpdateWatchSettings(s Settings, o ExportOptions) {
	a.wMu.Lock()
	a.wSettings = stripImageSpecific(normalizeSettings(s))
	a.wOpts = normalizeOptions(o)
	a.wOpts.OutName = ""
	a.wMu.Unlock()
}

// StartWatch begins polling cfg.Inbox; new images are processed into cfg.Outbox.
func (a *App) StartWatch(cfg WatchConfig, s Settings, o ExportOptions) WatchStatus {
	inbox, outbox := normalizePath(cfg.Inbox), normalizePath(cfg.Outbox)
	a.wMu.Lock()
	if a.w != nil {
		st := a.watchStatusLocked()
		a.wMu.Unlock()
		return st
	}
	a.wErr = ""
	switch {
	case inbox == "" || outbox == "":
		a.wErr = "Choose both an Inbox and an Output folder"
	case filepath.Clean(inbox) == filepath.Clean(outbox):
		a.wErr = "Inbox and Output folders must be different"
	default:
		if fi, err := os.Stat(inbox); err != nil || !fi.IsDir() {
			a.wErr = "Inbox folder does not exist"
		} else if err := os.MkdirAll(outbox, 0o755); err != nil {
			a.wErr = "Cannot create the Output folder: " + err.Error()
		}
	}
	if a.wErr != "" {
		st := a.watchStatusLocked()
		a.wMu.Unlock()
		return st
	}
	cfg.Inbox, cfg.Outbox = inbox, outbox
	a.wSettings = stripImageSpecific(normalizeSettings(s))
	a.wOpts = normalizeOptions(o)
	a.wOpts.OutName = ""
	a.wProc, a.wFail, a.wLast = 0, 0, ""
	ctx, cancel := context.WithCancel(context.Background())
	w := &watcher{cancel: cancel, done: make(chan struct{}), cfg: cfg}
	a.w = w
	st := a.watchStatusLocked()
	a.wMu.Unlock()

	go a.watchLoop(ctx, w)
	return st
}

// StopWatch stops the watcher and waits (bounded) for the goroutine to exit.
func (a *App) StopWatch() WatchStatus {
	a.wMu.Lock()
	w := a.w
	a.wMu.Unlock()
	if w != nil {
		w.cancel()
		select {
		case <-w.done:
		case <-time.After(15 * time.Second):
		}
		a.wMu.Lock()
		if a.w == w {
			a.w = nil
		}
		a.wMu.Unlock()
		debug.FreeOSMemory()
	}
	return a.WatchStatus()
}

func (a *App) watchLoop(ctx context.Context, w *watcher) {
	defer close(w.done)
	defer func() { _ = recover() }()
	handled := map[string]fileSig{}
	pending := map[string]fileSig{}
	first := true
	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()

	scan := func() {
		present := map[string]bool{}
		for _, p := range listImages(w.cfg.Inbox) {
			if ctx.Err() != nil {
				return
			}
			sig, ok := sigOf(p)
			if !ok {
				continue
			}
			present[p] = true
			if handled[p] == sig {
				continue
			}
			if first && !w.cfg.ProcessExisting {
				handled[p] = sig
				continue
			}
			if pending[p] != sig { // wait until the copy has finished (size/mtime stable)
				pending[p] = sig
				continue
			}
			delete(pending, p)
			a.wMu.Lock()
			s, o := a.wSettings, a.wOpts
			a.wMu.Unlock()
			o.OutDir = w.cfg.Outbox
			o.Kind = "ink"
			files, _, err := exportFile(p, s, o, nil)
			handled[p] = sig
			a.wMu.Lock()
			evt := map[string]interface{}{"file": filepath.Base(p)}
			if err != nil {
				a.wFail++
				a.wLast = filepath.Base(p) + ": " + err.Error()
				evt["status"], evt["message"] = "error", err.Error()
			} else {
				a.wProc++
				a.wLast = filepath.Base(files[0])
				evt["status"], evt["message"] = "ok", filepath.Base(files[0])
			}
			evt["processed"], evt["failed"] = a.wProc, a.wFail
			a.wMu.Unlock()
			a.emit("watch-event", evt)
		}
		first = false
		for p := range handled { // forget files that left the inbox
			if !present[p] {
				delete(handled, p)
			}
		}
		for p := range pending {
			if !present[p] {
				delete(pending, p)
			}
		}
	}
	for {
		scan()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ===========================================================================
// Output folder, ZIP
// ===========================================================================

func openDirectory(dir string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer", dir)
	case "darwin":
		cmd = exec.Command("open", dir)
	default:
		cmd = exec.Command("xdg-open", dir)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }() // reap the process; explorer exits non-zero even on success
	return nil
}

// OpenFolder opens any existing folder in the system file manager.
func (a *App) OpenFolder(dir string) {
	d := normalizePath(dir)
	if d == "" {
		return
	}
	if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
		d = filepath.Dir(d)
	}
	_ = openDirectory(d)
}

func (a *App) OpenOutFolder(basePath string) {
	p := normalizePath(basePath)
	if p == "" {
		return
	}
	outDir := p
	if fi, err := os.Stat(p); err == nil && fi.IsDir() && filepath.Base(p) != outDirName {
		outDir = filepath.Join(p, outDirName)
	} else if err != nil || !fi.IsDir() {
		outDir = resolveOutDir(p, "")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return
	}
	_ = openDirectory(outDir)
}

func addFileToZip(zw *zip.Writer, path, name string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	// PNG / JPEG / PDF are already compressed, so Store is faster and no larger.
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store, Modified: fi.ModTime()})
	if err != nil {
		return err
	}
	_, err = io.Copy(w, f)
	return err
}

// ExportZip bundles <folder>/PhotoBG_Output/* into <folder>/PhotoBG_Export.zip.
// The archive is written to a temp file and only moved into place once fully closed.
func (a *App) ExportZip(basePath string) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			msg = fmt.Sprintf("Error: %v", r)
		}
	}()
	p := normalizePath(basePath)
	if p == "" {
		return "Error: Add photos first"
	}
	var outDir, dir string
	if fi, err := os.Stat(p); err == nil && fi.IsDir() && filepath.Base(p) == outDirName {
		outDir, dir = p, filepath.Dir(p)
	} else {
		outDir = resolveOutDir(p, "")
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			outDir = filepath.Join(p, outDirName)
		}
		dir = filepath.Dir(outDir)
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return "Error: No processed images yet. Click Process All first"
	}
	var names []string
	for _, e := range entries {
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".png", ".svg", ".pdf", ".jpg":
			if !e.IsDir() {
				names = append(names, e.Name())
			}
		}
	}
	if len(names) == 0 {
		return "Error: No processed images yet. Click Process All first"
	}
	sort.Strings(names)

	tmp, err := os.CreateTemp(dir, "PhotoBG_Export_*.tmp")
	if err != nil {
		return "Error: " + err.Error()
	}
	tmpName := tmp.Name()
	fail := func(e error) string {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "Error: " + e.Error()
	}
	zw := zip.NewWriter(tmp)
	for _, n := range names {
		if err := addFileToZip(zw, filepath.Join(outDir, n), n); err != nil {
			_ = zw.Close()
			return fail(err)
		}
	}
	if err := zw.Close(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "Error: " + err.Error()
	}
	_ = os.Chmod(tmpName, 0o644)

	final := filepath.Join(dir, zipFileName)
	if err := os.Rename(tmpName, final); err != nil {
		placed := false
		for i := 1; i < 1000 && !placed; i++ {
			alt := filepath.Join(dir, fmt.Sprintf("PhotoBG_Export_%d.zip", i))
			if _, statErr := os.Stat(alt); statErr == nil {
				continue
			}
			if os.Rename(tmpName, alt) == nil {
				final, placed = alt, true
			}
		}
		if !placed {
			_ = os.Remove(tmpName)
			return "Error: Could not write the ZIP file"
		}
	}
	return fmt.Sprintf("Exported %d files to %s", len(names), filepath.Base(final))
}

// ===========================================================================
// GitHub update checker (only when the user clicks the button)
// ===========================================================================

func parseVersion(v string) [3]int {
	v = strings.TrimPrefix(strings.TrimSpace(strings.ToLower(v)), "v")
	if i := strings.IndexAny(v, "-+ "); i >= 0 {
		v = v[:i]
	}
	var out [3]int
	for i, part := range strings.SplitN(v, ".", 3) {
		out[i], _ = strconv.Atoi(part)
	}
	return out
}

func newerVersion(latest, current string) bool {
	l, c := parseVersion(latest), parseVersion(current)
	for i := 0; i < 3; i++ {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

func (a *App) CheckForUpdates() UpdateInfo {
	info := UpdateInfo{Current: appVersion}
	client := &http.Client{Timeout: 8 * time.Second}
	req, err := http.NewRequest("GET", "https://api.github.com/repos/"+repoOwner+"/"+repoName+"/releases/latest", nil)
	if err != nil {
		info.Error = err.Error()
		return info
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "PhotoBG/"+appVersion)
	resp, err := client.Do(req)
	if err != nil {
		info.Error = "Could not reach GitHub. Check your internet connection."
		return info
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		info.Latest = appVersion
		return info // no release published yet
	}
	if resp.StatusCode != http.StatusOK {
		info.Error = fmt.Sprintf("GitHub returned status %d", resp.StatusCode)
		return info
	}
	var rel struct {
		Tag  string `json:"tag_name"`
		URL  string `json:"html_url"`
		Body string `json:"body"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		info.Error = "Unexpected response from GitHub"
		return info
	}
	info.Latest = strings.TrimPrefix(rel.Tag, "v")
	info.URL = rel.URL
	if rs := []rune(rel.Body); len(rs) > 600 {
		info.Notes = string(rs[:600]) + "..."
	} else {
		info.Notes = rel.Body
	}
	info.UpdateAvailable = newerVersion(rel.Tag, appVersion)
	return info
}

// ===========================================================================
// Built-in demo sample (synthetic signature + red stamp, generated in pure Go)
// ===========================================================================

// GetDemoSample generates (once) a realistic scanned-page sample and returns its path.
func (a *App) GetDemoSample() (path string) {
	defer func() {
		if r := recover(); r != nil {
			path = ""
		}
	}()
	p := filepath.Join(appTempDir("paste"), "demo_sample.png")
	if fileExists(p) {
		return p
	}
	img := generateDemo()
	data, err := encodePNG(img, png.BestSpeed)
	if err != nil {
		return ""
	}
	if err := writeFileAtomic(p, data); err != nil {
		return ""
	}
	return p
}

func catmull(p0, p1, p2, p3 fpt, t float64) fpt {
	t2, t3 := t*t, t*t*t
	f := func(a, b, c, d float64) float64 {
		return 0.5 * ((2 * b) + (-a+c)*t + (2*a-5*b+4*c-d)*t2 + (-a+3*b-3*c+d)*t3)
	}
	return fpt{f(p0.x, p1.x, p2.x, p3.x), f(p0.y, p1.y, p2.y, p3.y)}
}

func generateDemo() *image.NRGBA {
	const W, H = 1000, 620
	rng := rand.New(rand.NewSource(20261127))
	sig := make([]float32, W*H)
	stp := make([]float32, W*H)

	disc := func(buf []float32, cx, cy, r float64) {
		for y := int(cy - r - 1); y <= int(cy+r+1); y++ {
			for x := int(cx - r - 1); x <= int(cx+r+1); x++ {
				if x < 0 || y < 0 || x >= W || y >= H {
					continue
				}
				d := math.Hypot(float64(x)+0.5-cx, float64(y)+0.5-cy)
				c := float32(math.Max(0, math.Min(1, r+0.5-d)))
				if c > buf[y*W+x] {
					buf[y*W+x] = c
				}
			}
		}
	}
	spline := func(pts []fpt, r0, r1 float64) {
		n := len(pts)
		for i := 0; i < n-1; i++ {
			p0, p3 := pts[max(i-1, 0)], pts[min(i+2, n-1)]
			for k := 0; k < 40; k++ {
				t := float64(k) / 40
				q := catmull(p0, pts[i], pts[i+1], p3, t)
				prog := (float64(i) + t) / float64(n-1)
				disc(sig, q.x, q.y, r0+(r1-r0)*math.Sin(prog*math.Pi)+0.25*math.Sin(prog*61))
			}
		}
	}
	mapPts := func(raw [][2]float64, x0, y0, w, h float64) []fpt {
		out := make([]fpt, len(raw))
		for i, r := range raw {
			out[i] = fpt{x0 + r[0]*w, y0 + r[1]*h}
		}
		return out
	}
	spline(mapPts([][2]float64{{0, .62}, {.05, .3}, {.1, .02}, {.14, .45}, {.18, .9}, {.24, .55}, {.3, .22}, {.34, .66}, {.4, .74},
		{.46, .38}, {.52, .28}, {.56, .72}, {.63, .78}, {.68, .45}, {.74, .26}, {.8, .62}, {.88, .82}, {.96, .52}, {1, .38}},
		90, 300, 520, 190), 2.0, 3.4)
	spline(mapPts([][2]float64{{.02, .97}, {.35, .9}, {.7, .96}, {1, .88}}, 90, 300, 540, 190), 1.4, 2.2)

	// Stamp: upright analytic rings + text masks, rotated by -12 degrees.
	type tm struct {
		m      *image.Alpha
		dx, dy float64
	}
	var texts []tm
	for _, t := range []struct {
		s  string
		px float64
		dy float64
	}{{"OFFICIAL SEAL", 27, -16}, {"PHOTO BG", 38, 22}, {"VERIFIED", 20, 66}} {
		if m, err := renderTextMask(t.s, t.px); err == nil {
			texts = append(texts, tm{m, -float64(m.Rect.Dx()) / 2, t.dy - float64(m.Rect.Dy())/2})
		}
	}
	scx, scy := 700.0, 330.0
	const R = 132.0
	ang := -12 * math.Pi / 180
	ca, sa := math.Cos(ang), math.Sin(ang)
	for y := int(scy - R - 6); y <= int(scy+R+6); y++ {
		for x := int(scx - R - 6); x <= int(scx+R+6); x++ {
			if x < 0 || y < 0 || x >= W || y >= H {
				continue
			}
			dx, dy := float64(x)+0.5-scx, float64(y)+0.5-scy
			u, v := dx*ca+dy*sa, -dx*sa+dy*ca
			d := math.Hypot(u, v)
			c := 0.0
			ring := func(r, wd float64) {
				c = math.Max(c, math.Max(0, math.Min(1, wd/2+0.5-math.Abs(d-r))))
			}
			ring(R, 8)
			ring(R-20, 3)
			ring(R-74, 2)
			for _, t := range texts {
				tx, ty := int(u-t.dx), int(v-t.dy)
				if tx >= 0 && ty >= 0 && tx < t.m.Rect.Dx() && ty < t.m.Rect.Dy() {
					c = math.Max(c, float64(t.m.Pix[ty*t.m.Stride+tx])/255)
				}
			}
			if c > 0 {
				c *= 0.62 + 0.38*rng.Float64() // uneven stamp-pad ink
				stp[y*W+x] = float32(c)
			}
		}
	}

	img := image.NewNRGBA(image.Rect(0, 0, W, H))
	for y := 0; y < H; y++ {
		for x := 0; x < W; x++ {
			light := 1.0 - 0.17*math.Pow(float64(x)/W, 1.6) - 0.12*math.Pow(float64(y)/H, 2.2)
			if x+y > 1250 { // soft fold shadow
				light -= 0.05 * math.Min(1, float64(x+y-1250)/300)
			}
			n := 1 + (rng.Float64()-0.5)*0.035
			r, g, b := 246*light*n, 244*light*n, 236*light*n
			if s := float64(sig[y*W+x]); s > 0 { // multiply-blend inks
				k := s * 0.93
				r *= 1 - k*(1-22.0/255)
				g *= 1 - k*(1-52.0/255)
				b *= 1 - k*(1-165.0/255)
			}
			if s := float64(stp[y*W+x]); s > 0 {
				k := s * 0.85
				r *= 1 - k*(1-205.0/255)
				g *= 1 - k*(1-28.0/255)
				b *= 1 - k*(1-52.0/255)
			}
			o := img.PixOffset(x, y)
			img.Pix[o], img.Pix[o+1], img.Pix[o+2], img.Pix[o+3] = uint8(math.Min(255, r)), uint8(math.Min(255, g)), uint8(math.Min(255, b)), 255
		}
	}
	for i := 0; i < 36; i++ { // scanner dust
		cx, cy := rng.Float64()*W, rng.Float64()*H
		r := 0.7 + rng.Float64()*1.4
		for y := int(cy - 3); y <= int(cy+3); y++ {
			for x := int(cx - 3); x <= int(cx+3); x++ {
				if x < 0 || y < 0 || x >= W || y >= H {
					continue
				}
				if c := math.Max(0, math.Min(1, r+0.5-math.Hypot(float64(x)-cx, float64(y)-cy))); c > 0 {
					o := img.PixOffset(x, y)
					for k := 0; k < 3; k++ {
						img.Pix[o+k] = uint8(float64(img.Pix[o+k]) * (1 - 0.55*c))
					}
				}
			}
		}
	}
	return img
}

// ===========================================================================
// Headless CLI mode:  Photo_BG.exe --in input.jpg --out output.png --ink blue
// ===========================================================================

func isCLIInvocation(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			return true
		}
	}
	return false
}

func runCLI(args []string) int {
	fset := flag.NewFlagSet("Photo_BG", flag.ContinueOnError)
	fset.SetOutput(os.Stderr)
	def := defaultSettings()
	in := fset.String("in", "", "input image file or folder (folders are scanned recursively)")
	out := fset.String("out", "", "output file (.png/.svg/.pdf/.jpg) or folder; default: <input folder>/"+outDirName)
	ink := fset.String("ink", "original", "original | black | blue | red | #RRGGBB")
	sep := fset.String("sep", "all", "all | signature | stamp | split (writes *_signature and *_stamp)")
	transp := fset.Int("transparency", def.Transparency, "background removal strength 0-100")
	bold := fset.Int("boldness", def.Boldness, "edge boldness 0-100")
	stroke := fset.Int("stroke", def.Stroke, "stroke boldness -2 (thin) .. 3 (thick)")
	desp := fset.Int("despeckle", def.Despeckle, "dust cleaner 0 (off) .. 10")
	trim := fset.Bool("trim", true, "auto-trim to the ink bounding box")
	pad := fset.Int("pad", def.Padding, "padding in px around the trim (0-64)")
	deskew := fset.Bool("deskew", false, "auto-straighten tilted scans")
	up := fset.Int("upscale", def.Upscale, "HD upscale factor 1-3")
	rot := fset.Int("rotate", 0, "rotate clockwise 0 | 90 | 180 | 270")
	format := fset.String("format", "png", "comma list of: png,svg,pdf,jpg")
	svg := fset.Bool("svg", false, "also write a vector SVG")
	pdf := fset.Bool("pdf", false, "also write a PDF")
	doc := fset.Bool("doc", false, "export the full cleaned document page instead of a transparent layer")
	maxkb := fset.Int("maxkb", 0, "size limit: 500 (<500KB) or 1000 (<1MB); 0 = original HD")
	wmText := fset.String("watermark", "", "watermark text ({date} = today)")
	quiet := fset.Bool("quiet", false, "print nothing on success")
	ver := fset.Bool("version", false, "print the version and exit")
	if err := fset.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *ver {
		fmt.Println("Photo BG " + appVersion)
		return 0
	}
	if strings.TrimSpace(*in) == "" {
		fmt.Fprintln(os.Stderr, "Photo BG: --in is required (see --help)")
		return 2
	}
	files := collectImages([]string{*in}, 0)
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "Photo BG: no supported images found in", *in)
		return 1
	}

	s := def
	s.Ink = *ink
	s.Transparency, s.Boldness, s.Stroke, s.Despeckle = *transp, *bold, *stroke, *desp
	s.AutoTrim, s.Padding, s.Deskew, s.Upscale, s.Rotate = *trim, *pad, *deskew, *up, *rot
	split := false
	switch strings.ToLower(*sep) {
	case "split":
		split = true
	case "signature", "stamp", "all":
		s.Separator = strings.ToLower(*sep)
	}
	switch {
	case *maxkb > 0 && *maxkb <= 500:
		s.SizeMode = "kb500"
	case *maxkb > 500 && *maxkb <= 1000:
		s.SizeMode = "mb1"
	}
	if strings.TrimSpace(*wmText) != "" {
		s.Watermark.Enabled, s.Watermark.Text = true, *wmText
	}

	o := ExportOptions{Kind: "ink", Split: split, CleanDoc: true}
	if *doc {
		o.Kind = "document"
	}
	o.Formats = strings.Split(*format, ",")
	if *svg {
		o.Formats = append(o.Formats, "svg")
	}
	if *pdf {
		o.Formats = append(o.Formats, "pdf")
	}

	if *out != "" {
		op := normalizePath(*out)
		switch strings.ToLower(filepath.Ext(op)) {
		case ".png", ".svg", ".pdf", ".jpg", ".jpeg":
			if len(files) == 1 {
				o.OutDir = filepath.Dir(op)
				o.OutName = strings.TrimSuffix(filepath.Base(op), filepath.Ext(op))
				if e := strings.ToLower(strings.TrimPrefix(filepath.Ext(op), ".")); e != "" && *format == "png" && !*svg && !*pdf {
					o.Formats = []string{e}
				}
			} else {
				fmt.Fprintln(os.Stderr, "Photo BG: --out must be a folder when --in has several images")
				return 2
			}
		default:
			o.OutDir = op
		}
	}

	bud := newPixelBudget(decodeBudgetPx)
	failed := 0
	for _, f := range files {
		written, _, err := exportFile(f, s, o, bud)
		if err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "Photo BG: %s: %v\n", filepath.Base(f), err)
			continue
		}
		if !*quiet {
			for _, w := range written {
				fmt.Println(w)
			}
		}
	}
	debug.FreeOSMemory()
	if failed > 0 {
		return 1
	}
	return 0
}
