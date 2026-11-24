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

type App struct {
	ctx context.Context
}

func NewApp() *App                         { return &App{} }
func (a *App) startup(ctx context.Context) { a.ctx = ctx }

func (a *App) GetDefaultFolder() string {
	ss := filepath.Join(os.Getenv("USERPROFILE"), "Pictures", "Screenshots")
	if _, err := os.Stat(ss); err == nil {
		return ss
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return ""
}

func (a *App) SelectFilesOrFolder() string {
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
	folder = strings.Trim(strings.TrimSpace(folder), "\"")
	entries, err := os.ReadDir(folder)
	if err != nil {
		return []string{}
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext == ".png" || ext == ".jpg" || ext == ".jpeg" || ext == ".webp" || ext == ".bmp" {
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
	fullPath := filepath.Join(strings.Trim(strings.TrimSpace(folder), "\""), filename)
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
	work := src
	if b.Dx() > 850 {
		ratio := 850.0 / float64(b.Dx())
		dst := image.NewRGBA(image.Rect(0, 0, 500, int(float64(b.Dy())*ratio)))
		xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, b, xdraw.Over, nil)
		work = dst
	}

	var bufOrig bytes.Buffer
	png.Encode(&bufOrig, work)

	cleanImg, err := extractSig(work, colorMode, boldness, false)
	if err != nil {
		return PreviewResult{OriginalB64: "data:image/png;base64," + base64.StdEncoding.EncodeToString(bufOrig.Bytes()), Error: err.Error()}
	}
	var bufClean bytes.Buffer
	png.Encode(&bufClean, cleanImg)

	return PreviewResult{
		OriginalB64:  "data:image/png;base64," + base64.StdEncoding.EncodeToString(bufOrig.Bytes()),
		ExtractedB64: "data:image/png;base64," + base64.StdEncoding.EncodeToString(bufClean.Bytes()),
	}
}

func (a *App) ExtractAll(folder, colorMode string, boldness int, formSize bool) string {
	folder = strings.Trim(strings.TrimSpace(folder), "\"")
	files := a.ListImages(folder)
	if len(files) == 0 {
		return "No images found in this folder!"
	}
	outDir := filepath.Join(folder, "clean_signatures")
	os.MkdirAll(outDir, 0755)

	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())
	var okCount int32
	var mu sync.Mutex

	for _, fn := range files {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

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
			outName := strings.TrimSuffix(name, filepath.Ext(name)) + ".png"
			of, err := os.Create(filepath.Join(outDir, outName))
			if err == nil {
				png.Encode(of, outImg)
				of.Close()
				mu.Lock()
				okCount++
				mu.Unlock()
			}
		}(fn)
	}
	wg.Wait()
	exec.Command("explorer", outDir).Start()
	return fmt.Sprintf("Done! %d / %d signatures extracted cleanly into clean_signatures!", okCount, len(files))
}

func (a *App) OpenOutFolder(folder string) {
	outDir := filepath.Join(strings.Trim(strings.TrimSpace(folder), "\""), "clean_signatures")
	os.MkdirAll(outDir, 0755)
	exec.Command("explorer", outDir).Start()
}

func (a *App) ExportZip(folder string) string {
	outDir := filepath.Join(strings.Trim(strings.TrimSpace(folder), "\""), "clean_signatures")
	entries, err := os.ReadDir(outDir)
	if err != nil || len(entries) == 0 {
		return "Extract signatures first before creating ZIP!"
	}
	zipPath := filepath.Join(strings.Trim(strings.TrimSpace(folder), "\""), "clean_signatures.zip")
	zf, err := os.Create(zipPath)
	if err != nil {
		return err.Error()
	}
	defer zf.Close()
	zw := zip.NewWriter(zf)
	defer zw.Close()

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		f, err := os.Open(filepath.Join(outDir, e.Name()))
		if err != nil {
			continue
		}
		w, _ := zw.Create(e.Name())
		io.Copy(w, f)
		f.Close()
	}
	return "ZIP Created: clean_signatures.zip"
}

func extractSig(src image.Image, colorMode string, boldness int, formSize bool) (*image.NRGBA, error) {
	sb := src.Bounds()
	w, h := sb.Dx()*3, sb.Dy()*3
	up := image.NewRGBA(image.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(up, up.Bounds(), src, sb, xdraw.Over, nil)

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
			sum := integral[(y2+1)*(w+1)+(x2+1)] - integral[y1*(w+1)+(x2+1)] - integral[(y2+1)*(w+1)+x1] + integral[y1*(w+1)+x1]
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
	out := image.NewNRGBA(image.Rect(0, 0, cw, ch))

	for y := 0; y < ch; y++ {
		for x := 0; x < cw; x++ {
			a := alpha[(minY+y)*w+(minX+x)]
			if colorMode == "blue" {
				out.SetNRGBA(x, y, color.NRGBA{15, 55, 195, a})
			} else if colorMode == "original" {
				c := up.RGBAAt(minX+x, minY+y)
				out.SetNRGBA(x, y, color.NRGBA{c.R, c.G, c.B, a})
			} else {
				out.SetNRGBA(x, y, color.NRGBA{0, 0, 0, a})
			}
		}
	}

	if formSize {
		std := image.NewNRGBA(image.Rect(0, 0, 600, 200))
		scale := math.Min(560.0/float64(cw), 170.0/float64(ch))
		dw, dh := int(float64(cw)*scale), int(float64(ch)*scale)
		dx, dy := (600-dw)/2, (200-dh)/2
		xdraw.CatmullRom.Scale(std, image.Rect(dx, dy, dx+dw, dy+dh), out, out.Bounds(), xdraw.Over, nil)
		return std, nil
	}
	return out, nil
}

func (a *App) SaveOne(folder, filename, colorMode string, boldness int, formSize bool) string {
	folder = strings.Trim(strings.TrimSpace(folder), "\"")
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
	os.MkdirAll(outDir, 0755)
	outName := strings.TrimSuffix(filename, filepath.Ext(filename)) + "_clean.png"
	outPath := filepath.Join(outDir, outName)
	of, err := os.Create(outPath)
	if err != nil {
		return "Error: " + err.Error()
	}
	png.Encode(of, outImg)
	of.Close()
	return "Saved: clean_signatures\\" + outName
}
