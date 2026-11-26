package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	"image/png"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	wailsrt "github.com/wailsapp/wails/v2/pkg/runtime"
	_ "golang.org/x/image/bmp"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	maxSrcDim  = 1200 // cap source before 3x upscale (memory safety)
	maxWorkers = 4    // cap concurrent extractors (memory safety)
	previewMax = 850
	formWidth  = 600
	formHeight = 200
)

type App struct {
	ctx context.Context
}

func NewApp() *App { return &App{} }

// CRITICAL FIX: Wails v2 only auto-detects exported "OnStartup".
func (a *App) OnStartup(ctx context.Context) { a.ctx = ctx }

func cleanFolder(folder string) string {
	return strings.Trim(strings.TrimSpace(folder), "\"")
}

func isImageExt(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".webp", ".bmp":
		return true
	}
	return false
}

func (a *App) GetDefaultFolder() string {
	if profile := os.Getenv("USERPROFILE"); profile != "" {
		ss := filepath.Join(profile, "Pictures", "Screenshots")
		if st, err := os.Stat(ss); err == nil && st.IsDir() {
			return ss
		}
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return ""
}

func (a *App) SelectFilesOrFolder() string {
	if a.ctx == nil {
		return ""
	}
	files, err := wailsrt.OpenMultipleFilesDialog(a.ctx, wailsrt.OpenDialogOptions{
		Title: "Select Signature Images (or paste folder path in address bar above)",
		Filters: []wailsrt.FileFilter{
			{DisplayName: "Signature Images (*.png, *.jpg, *.jpeg, *.webp, *.bmp)", Pattern: "*.png;*.jpg;*.jpeg;*.webp;*.bmp"},
		},
	})
	if err == nil && len(files) > 0 {
		return filepath.Dir(files[0])
	}
	return ""
}

func (a *App) ListImages(folder string) []string {
	folder = cleanFolder(folder)
	entries, err := os.ReadDir(folder)
	if err != nil {
		return []string{}
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if isImageExt(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

type PreviewResult struct {
	OriginalB64  string `json:"originalB64"`
	ExtractedB64 string `json:"extractedB64"`
	Error        string `json:"error"`
}

func (a *App) GetPreview(folder, filename, colorMode string, boldness int) PreviewResult {
	fullPath := filepath.Join(cleanFolder(folder), filename)
	f, err := os.Open(fullPath)
	if err != nil {
		return PreviewResult{Error: err.Error()}
	}
	defer f.Close()
	src, _, err := image.Decode(f)
	if err != nil {
		return PreviewResult{Error: err.Error()}
	}

	b := src.Bounds()
	if b.Dx() == 0 || b.Dy() == 0 {
		return PreviewResult{Error: "empty image"}
	}

	// FIX #2: correct aspect ratio (was 500, must match ratio base of 850).
	var work image.Image = src
	if b.Dx() > previewMax || b.Dy() > previewMax {
		ratio := math.Min(float64(previewMax)/float64(b.Dx()), float64(previewMax)/float64(b.Dy()))
		nw := max(1, int(float64(b.Dx())*ratio))
		nh := max(1, int(float64(b.Dy())*ratio))
		dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
		xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, b, xdraw.Src, nil)
		work = dst
	}

	var bufOrig bytes.Buffer
	if err := png.Encode(&bufOrig, work); err != nil {
		return PreviewResult{Error: "encode original: " + err.Error()}
	}
	origB64 := "data:image/png;base64," + base64.StdEncoding.EncodeToString(bufOrig.Bytes())

	cleanImg, err := extractSig(work, colorMode, boldness, false)
	if err != nil {
		return PreviewResult{OriginalB64: origB64, Error: err.Error()}
	}
	var bufClean bytes.Buffer
	if err := png.Encode(&bufClean, cleanImg); err != nil {
		return PreviewResult{OriginalB64: origB64, Error: "encode cleaned: " + err.Error()}
	}

	return PreviewResult{
		OriginalB64:  origB64,
		ExtractedB64: "data:image/png;base64," + base64.StdEncoding.EncodeToString(bufClean.Bytes()),
	}
}

func (a *App) ExtractAll(folder, colorMode string, boldness int, formSize bool) string {
	folder = cleanFolder(folder)
	files := a.ListImages(folder)
	if len(files) == 0 {
		return "No images found in this folder!"
	}
	outDir := filepath.Join(folder, "clean_signatures")
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return "Cannot create output folder: " + err.Error()
	}

	// FIX #4: cap workers — memory-safe.
	workers := runtime.NumCPU()
	if workers > maxWorkers {
		workers = maxWorkers
	}
	if workers < 1 {
		workers = 1
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	okCount, failCount := 0, 0

	for _, fn := range files {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			ok := false
			defer func() {
				mu.Lock()
				if ok {
					okCount++
				} else {
					failCount++
				}
				mu.Unlock()
			}()

			inP := filepath.Join(folder, name)
			f, err := os.Open(inP)
			if err != nil {
				return
			}
			src, _, err := image.Decode(f)
			f.Close()
			if err != nil {
				return
			}
			outImg, err := extractSig(src, colorMode, boldness, formSize)
			if err != nil {
				return
			}

			// FIX #5: collision-safe output name (sig.jpg + sig.png no longer overwrite).
			base := strings.TrimSuffix(name, filepath.Ext(name))
			for i := 0; i < 1000; i++ {
				on := base + ".png"
				if i > 0 {
					on = fmt.Sprintf("%s_%d.png", base, i)
				}
				outPath := filepath.Join(outDir, on)
				of, err := os.OpenFile(outPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
				if err != nil {
					if os.IsExist(err) {
						continue
					}
					return
				}
				if err := png.Encode(of, outImg); err != nil { // FIX #6
					of.Close()
					os.Remove(outPath)
					return
				}
				if err := of.Close(); err != nil {
					os.Remove(outPath)
					return
				}
				ok = true
				return
			}
		}(fn)
	}
	wg.Wait()

	_ = exec.Command("explorer", outDir).Start()

	msg := fmt.Sprintf("Done! %d / %d signatures extracted cleanly into clean_signatures!", okCount, len(files))
	if failCount > 0 {
		msg += fmt.Sprintf(" — %d failed/skipped.", failCount)
	}
	return msg
}

func (a *App) OpenOutFolder(folder string) {
	outDir := filepath.Join(cleanFolder(folder), "clean_signatures")
	_ = os.MkdirAll(outDir, 0755)
	_ = exec.Command("explorer", outDir).Start()
}

func (a *App) ExportZip(folder string) string {
	root := cleanFolder(folder)
	outDir := filepath.Join(root, "clean_signatures")
	entries, err := os.ReadDir(outDir)
	if err != nil || len(entries) == 0 {
		return "Extract signatures first before creating ZIP!"
	}
	zipPath := filepath.Join(root, "clean_signatures.zip")
	zf, err := os.Create(zipPath)
	if err != nil {
		return "Cannot create ZIP: " + err.Error()
	}

	zw := zip.NewWriter(zf)
	added := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		f, err := os.Open(filepath.Join(outDir, e.Name()))
		if err != nil {
			continue
		}
		w, err := zw.Create(e.Name()) // FIX #7
		if err != nil {
			f.Close()
			continue
		}
		if _, err := io.Copy(w, f); err == nil {
			added++
		}
		f.Close()
	}
	// FIX #8: explicit close with error handling (defer order + swallowing was wrong).
	if err := zw.Close(); err != nil {
		zf.Close()
		return "ZIP write failed: " + err.Error()
	}
	if err := zf.Close(); err != nil {
		return "ZIP close failed: " + err.Error()
	}
	if added == 0 {
		os.Remove(zipPath)
		return "No files could be added to ZIP."
	}
	return fmt.Sprintf("ZIP Created: clean_signatures.zip (%d files)", added)
}

func extractSig(src image.Image, colorMode string, boldness int, formSize bool) (*image.NRGBA, error) {
	sb := src.Bounds()
	if sb.Dx() == 0 || sb.Dy() == 0 {
		return nil, fmt.Errorf("empty image")
	}

	// FIX #3: cap source before 3x upscale — prevents multi-GB allocations.
	if sb.Dx() > maxSrcDim || sb.Dy() > maxSrcDim {
		ratio := math.Min(float64(maxSrcDim)/float64(sb.Dx()), float64(maxSrcDim)/float64(sb.Dy()))
		nw := max(1, int(float64(sb.Dx())*ratio))
		nh := max(1, int(float64(sb.Dy())*ratio))
		down := image.NewRGBA(image.Rect(0, 0, nw, nh))
		xdraw.CatmullRom.Scale(down, down.Bounds(), src, sb, xdraw.Src, nil)
		src = down
		sb = src.Bounds()
	}

	w, h := sb.Dx()*3, sb.Dy()*3
	up := image.NewRGBA(image.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(up, up.Bounds(), src, sb, xdraw.Src, nil)

	gray := make([]float64, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := up.RGBAAt(x, y)
			gray[y*w+x] = 0.299*float64(c.R) + 0.587*float64(c.G) + 0.114*float64(c.B)
		}
	}

	integral := make([]float64, (w+1)*(h+1))
	for y := 0; y < h; y++ {
		row := 0.0
		for x := 0; x < w; x++ {
			row += gray[y*w+x]
			integral[(y+1)*(w+1)+(x+1)] = integral[y*(w+1)+(x+1)] + row
		}
	}

	rad := 45
	thresh := 190.0 + (float64(boldness)/100.0)*40.0
	alpha := make([]uint8, w*h)
	minX, minY, maxX, maxY := w, h, 0, 0
	found := false

	for y := 0; y < h; y++ {
		y1, y2 := max(0, y-rad), min(h-1, y+rad)
		for x := 0; x < w; x++ {
			x1, x2 := max(0, x-rad), min(w-1, x+rad)
			area := float64((x2 - x1 + 1) * (y2 - y1 + 1))
			sum := integral[(y2+1)*(w+1)+(x2+1)] - integral[y1*(w+1)+(x2+1)] -
				integral[(y2+1)*(w+1)+x1] + integral[y1*(w+1)+x1]
			bg := math.Max(1.0, sum/area)
			diff := (gray[y*w+x] / bg) * 255.0
			aVal := ((244.0 - diff) / math.Max(10.0, 244.0-thresh)) * 255.0
			if aVal < 36 {
				aVal = 0
			} else if aVal > 255 {
				aVal = 255
			}
			ua := uint8(aVal)
			alpha[y*w+x] = ua
			if ua > 0 {
				found = true
				if x < minX {
					minX = x
				}
				if y < minY {
					minY = y
				}
				if x > maxX {
					maxX = x
				}
				if y > maxY {
					maxY = y
				}
			}
		}
	}
	if !found {
		return nil, fmt.Errorf("no ink detected")
	}

	pad := 24
	minX, minY = max(0, minX-pad), max(0, minY-pad)
	maxX, maxY = min(w-1, maxX+pad), min(h-1, maxY+pad)
	cw, ch := maxX-minX+1, maxY-minY+1
	if cw < 1 || ch < 1 {
		return nil, fmt.Errorf("empty extraction region")
	}
	out := image.NewNRGBA(image.Rect(0, 0, cw, ch))

	for y := 0; y < ch; y++ {
		for x := 0; x < cw; x++ {
			a := alpha[(minY+y)*w+(minX+x)]
			switch colorMode {
			case "blue":
				out.SetNRGBA(x, y, color.NRGBA{15, 55, 195, a})
			case "original":
				c := up.RGBAAt(minX+x, minY+y)
				out.SetNRGBA(x, y, color.NRGBA{c.R, c.G, c.B, a})
			default:
				out.SetNRGBA(x, y, color.NRGBA{0, 0, 0, a})
			}
		}
	}

	if formSize {
		std := image.NewNRGBA(image.Rect(0, 0, formWidth, formHeight))
		scale := math.Min(float64(formWidth-40)/float64(cw), float64(formHeight-30)/float64(ch))
		dw := max(1, int(float64(cw)*scale)) // FIX #12
		dh := max(1, int(float64(ch)*scale))
		dx, dy := (formWidth-dw)/2, (formHeight-dh)/2
		xdraw.CatmullRom.Scale(std, image.Rect(dx, dy, dx+dw, dy+dh), out, out.Bounds(), xdraw.Src, nil)
		return std, nil
	}
	return out, nil
}

func (a *App) SaveOne(folder, filename, colorMode string, boldness int, formSize bool) string {
	folder = cleanFolder(folder)
	inP := filepath.Join(folder, filename)
	f, err := os.Open(inP)
	if err != nil {
		return "Error: " + err.Error()
	}
	src, _, err := image.Decode(f)
	f.Close()
	if err != nil {
		return "Error: " + err.Error()
	}
	outImg, err := extractSig(src, colorMode, boldness, formSize)
	if err != nil {
		return "Error: " + err.Error()
	}
	outDir := filepath.Join(folder, "clean_signatures")
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return "Error: " + err.Error()
	}
	outName := strings.TrimSuffix(filename, filepath.Ext(filename)) + "_clean.png"
	outPath := filepath.Join(outDir, outName)
	of, err := os.Create(outPath)
	if err != nil {
		return "Error: " + err.Error()
	}
	if err := png.Encode(of, outImg); err != nil {
		of.Close()
		os.Remove(outPath)
		return "Error: " + err.Error()
	}
	if err := of.Close(); err != nil {
		os.Remove(outPath)
		return "Error: " + err.Error()
	}
	return "Saved: clean_signatures\\" + outName
}
