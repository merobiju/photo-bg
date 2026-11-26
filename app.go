package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
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
	"sync/atomic"

	wailsrt "github.com/wailsapp/wails/v2/pkg/runtime"
	_ "golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

const (
	outDirName  = "PhotoBG_Output"
	zipFileName = "PhotoBG_Export.zip"

	thumbMax      = 64   // longest side of queue thumbnails
	previewMax    = 900  // longest side of the live-preview source
	maxSrcDim     = 1200 // longest side of the export source before upscaling
	exportUpscale = 3    // HD export multiplier
	maxWorkers    = 4    // bounded worker pool for ProcessBatch
	maxDropImages = 500  // cap for dropped files/folders

	maxDecodePixels  = 120_000_000
	maxProcessPixels = 16_000_000

	bgRadius = 45 // local-background window radius (integral image)
	cropPad  = 6  // padding around the foreground bounding box
	formW    = 600
	formH    = 200

	previewCacheMax = 12
)

var imageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".bmp": true, ".webp": true, ".tif": true, ".tiff": true,
}

// PreviewResult is returned to the frontend by GetPreview.
// Clean/Orig are data URLs. (CropX, CropY) is where the clean image sits inside
// the OrigW x OrigH original preview, so the compare view can stay aligned.
type PreviewResult struct {
	Clean string `json:"clean"`
	Orig  string `json:"orig"`
	W     int    `json:"w"`
	H     int    `json:"h"`
	OrigW int    `json:"origW"`
	OrigH int    `json:"origH"`
	CropX int    `json:"cropX"`
	CropY int    `json:"cropY"`
	Error string `json:"error"`
}

type previewSrc struct {
	img  *image.NRGBA // flattened on white, capped at previewMax
	orig string       // JPEG data URL of img (encoded once)
}

// App is the Wails-bound backend.
type App struct {
	ctx      context.Context
	dropOnce sync.Once

	mu    sync.Mutex
	cache map[string]*previewSrc
	order []string
}

func NewApp() *App {
	return &App{cache: make(map[string]*previewSrc)}
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

func (a *App) OnStartup(ctx context.Context) {
	a.ctx = ctx
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

// GetDefaultFolder intentionally returns "" so the app always opens clean.
func (a *App) GetDefaultFolder() string {
	return ""
}

// ---------------------------------------------------------------------------
// Path / file selection
// ---------------------------------------------------------------------------

func (a *App) SelectFolder() []string {
	dir, err := wailsrt.OpenDirectoryDialog(a.ctx, wailsrt.OpenDialogOptions{
		Title: "Select a Folder of Photos",
	})
	if err != nil || dir == "" {
		return []string{}
	}
	return listImages(dir)
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

// ResolvePath accepts a pasted path (quotes/whitespace tolerated). A folder expands to
// its sorted images; a valid image file returns itself; anything else returns [].
func (a *App) ResolvePath(inputPath string) []string {
	return collectImages([]string{inputPath}, 0)
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

// collectImages expands files and folders into unique absolute image paths.
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
			for _, f := range listImages(p) {
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

// ---------------------------------------------------------------------------
// Image helpers
// ---------------------------------------------------------------------------

func decodeFile(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return nil, fmt.Errorf("Unsupported or damaged image file")
	}
	if cfg.Width < 1 || cfg.Height < 1 || int64(cfg.Width)*int64(cfg.Height) > maxDecodePixels {
		return nil, fmt.Errorf("Image dimensions are too large")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	img, _, err := image.Decode(bufio.NewReaderSize(f, 1<<16))
	if err != nil {
		return nil, fmt.Errorf("Could not decode image")
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

func opaqueSource(img image.Image) image.Image {
	if isOpaque(img) {
		return img
	}
	return flatten(img)
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

// loadCapped decodes a file and returns an opaque NRGBA whose longest side is <= maxDim.
func loadCapped(path string, maxDim int) (*image.NRGBA, error) {
	img, err := decodeFile(path)
	if err != nil {
		return nil, err
	}
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	nw, nh := fitDims(w, h, maxDim)
	src := opaqueSource(img)
	if nw == w && nh == h {
		return flatten(src), nil
	}
	return scaleInto(src, image.Rect(0, 0, nw, nh), nw, nh, draw.BiLinear), nil
}

func cropNRGBA(src *image.NRGBA, r image.Rectangle) *image.NRGBA {
	dst := image.NewNRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	rowLen := r.Dx() * 4
	for y := 0; y < r.Dy(); y++ {
		s := src.PixOffset(r.Min.X, r.Min.Y+y)
		d := dst.PixOffset(0, y)
		copy(dst.Pix[d:d+rowLen], src.Pix[s:s+rowLen])
	}
	return dst
}

func pngDataURL(img image.Image) (string, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, img); err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

func jpegDataURL(img *image.NRGBA, quality int) (string, error) {
	rgba := image.NewRGBA(img.Rect)
	draw.Draw(rgba, rgba.Rect, img, img.Rect.Min, draw.Src)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, rgba, &jpeg.Options{Quality: quality}); err != nil {
		return "", err
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ---------------------------------------------------------------------------
// Transparency engine
// ---------------------------------------------------------------------------

// extractImage removes the background of src and returns an NRGBA with computed alpha.
//
//	transparency 0..100: 0 keeps subtle tones, 100 removes background aggressively
//	boldness     0..100: edge contrast / threshold
//	autoCrop/formSize:   crop to the foreground bounding box (formSize also fits 600x200)
//	upscale:             HD multiplier (CatmullRom); ignored when formSize is true
func extractImage(src image.Image, colorMode string, transparency int, boldness int, autoCrop bool, formSize bool, upscale int) (*image.NRGBA, error) {
	img, _, err := extractWithCrop(src, colorMode, transparency, boldness, autoCrop, formSize, upscale)
	return img, err
}

// extractWithCrop is extractImage plus the crop rectangle (in source pixels) that
// was applied, which the live preview uses to keep the original aligned.
func extractWithCrop(src image.Image, colorMode string, transparency, boldness int, autoCrop, formSize bool, upscale int) (*image.NRGBA, image.Rectangle, error) {
	if src == nil {
		return nil, image.Rectangle{}, errors.New("No image to process")
	}
	transparency = clampInt(transparency, 0, 100)
	boldness = clampInt(boldness, 0, 100)
	upscale = clampInt(upscale, 1, 4)

	img := flatten(src)
	w, h := img.Rect.Dx(), img.Rect.Dy()
	if w < 1 || h < 1 {
		return nil, image.Rectangle{}, errors.New("Image is empty")
	}
	if int64(w)*int64(h) > maxProcessPixels {
		return nil, image.Rectangle{}, errors.New("Image is too large to process")
	}
	full := image.Rect(0, 0, w, h)

	// 1) Luminance (Rec. 601).
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

	// 2) Summed-area table (integral image) for O(1) local-mean lookups.
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

	// 3) Slider mapping.
	tf := float32(transparency) / 100
	bf := float32(boldness) / 100
	ceilL := 252 - tf*22        // normalized luminance at/above which a pixel is background
	minAlpha := 8 + tf*56       // alpha noise floor
	thresh := 185 + bf*45       // luminance at/below which ink is fully opaque
	span := ceilL - thresh      // soft edge width between "solid" and "background"
	if span < 1 {
		span = 1 // extreme settings collapse to a hard edge; never divide by zero
	}

	var cr, cg, cb uint8
	useOrig := true
	switch colorMode {
	case "black":
		useOrig = false
	case "blue":
		useOrig, cr, cg, cb = false, 15, 55, 195
	}

	// 4) Adaptive local-background normalization + alpha.
	out := image.NewNRGBA(full)
	op := out.Pix
	minX, minY, maxX, maxY := w, h, -1, -1

	for y := 0; y < h; y++ {
		y0 := max(0, y-bgRadius)
		y1 := min(h, y+bgRadius+1)
		for x := 0; x < w; x++ {
			x0 := max(0, x-bgRadius)
			x1 := min(w, x+bgRadius+1)
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
			ai := uint8(a + 0.5)
			if ai == 0 {
				continue
			}

			o := (y*w + x) * 4
			if useOrig {
				s := y*stride + x*4
				op[o], op[o+1], op[o+2] = pix[s], pix[s+1], pix[s+2]
			} else {
				op[o], op[o+1], op[o+2] = cr, cg, cb
			}
			op[o+3] = ai

			minX, maxX = min(minX, x), max(maxX, x)
			minY, maxY = min(minY, y), max(maxY, y)
		}
	}

	// 5) Nothing left? Return a clean transparent image, never an error, so live
	//    previews do not flash red while sliders are dragged to extremes.
	if maxX < 0 {
		if formSize {
			return image.NewNRGBA(image.Rect(0, 0, formW, formH)), full, nil
		}
		return image.NewNRGBA(image.Rect(0, 0, w, h)), full, nil
	}

	// 6) Framing: crop only on request; otherwise keep the exact frame.
	crop := full
	res := out
	if autoCrop || formSize {
		crop = image.Rect(minX-cropPad, minY-cropPad, maxX+1+cropPad, maxY+1+cropPad).Intersect(full)
		res = cropNRGBA(out, crop)
	}

	// 7) Output sizing.
	cw, ch := res.Rect.Dx(), res.Rect.Dy()
	if formSize {
		sc := math.Min(float64(formW)/float64(cw), float64(formH)/float64(ch))
		nw := min(formW, max(1, int(math.Round(float64(cw)*sc))))
		nh := min(formH, max(1, int(math.Round(float64(ch)*sc))))
		ox, oy := (formW-nw)/2, (formH-nh)/2
		res = scaleInto(res, image.Rect(ox, oy, ox+nw, oy+nh), formW, formH, draw.CatmullRom)
	} else if upscale > 1 {
		res = scaleInto(res, image.Rect(0, 0, cw*upscale, ch*upscale), cw*upscale, ch*upscale, draw.CatmullRom)
	}
	return res, crop, nil
}

// ---------------------------------------------------------------------------
// Thumbnails and live preview
// ---------------------------------------------------------------------------

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
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	nw, nh := fitDims(w, h, thumbMax)
	var thumb *image.NRGBA
	if nw == w && nh == h {
		thumb = flatten(img)
	} else {
		thumb = scaleInto(opaqueSource(img), image.Rect(0, 0, nw, nh), nw, nh, draw.ApproxBiLinear)
	}
	out, err := pngDataURL(thumb)
	if err != nil {
		return ""
	}
	return out
}

// previewSource returns the cached, flattened <=900px version of a photo.
func (a *App) previewSource(path string) (*previewSrc, error) {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return nil, errors.New("Photo not found")
	}
	key := fmt.Sprintf("%s|%d|%d", path, fi.Size(), fi.ModTime().UnixNano())

	a.mu.Lock()
	if ps, ok := a.cache[key]; ok {
		a.mu.Unlock()
		return ps, nil
	}
	a.mu.Unlock()

	img, err := loadCapped(path, previewMax)
	if err != nil {
		return nil, err
	}
	orig, err := jpegDataURL(img, 88)
	if err != nil {
		return nil, err
	}
	ps := &previewSrc{img: img, orig: orig}

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
	return ps, nil
}

// GetPreview renders the fast live preview (source capped at 900px, upscale = 1).
func (a *App) GetPreview(filePath, colorMode string, transparency, boldness int, autoCrop bool) (res PreviewResult) {
	defer func() {
		if r := recover(); r != nil {
			res = PreviewResult{Error: fmt.Sprintf("Preview failed: %v", r)}
		}
	}()
	filePath = normalizePath(filePath)
	if !isImageExt(filePath) {
		return PreviewResult{Error: "Not a supported image file"}
	}
	ps, err := a.previewSource(filePath)
	if err != nil {
		return PreviewResult{Error: err.Error()}
	}
	out, crop, err := extractWithCrop(ps.img, colorMode, transparency, boldness, autoCrop, false, 1)
	if err != nil {
		return PreviewResult{Error: err.Error()}
	}
	clean, err := pngDataURL(out)
	if err != nil {
		return PreviewResult{Error: err.Error()}
	}
	b := ps.img.Rect
	return PreviewResult{
		Clean: clean,
		Orig:  ps.orig,
		W:     out.Rect.Dx(),
		H:     out.Rect.Dy(),
		OrigW: b.Dx(),
		OrigH: b.Dy(),
		CropX: crop.Min.X,
		CropY: crop.Min.Y,
	}
}

// ---------------------------------------------------------------------------
// HD export
// ---------------------------------------------------------------------------

func savePNGUnique(outDir, stem string, img *image.NRGBA) (string, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	for i := 0; i < 10000; i++ {
		name := stem + "_PhotoBG.png"
		if i > 0 {
			name = fmt.Sprintf("%s_PhotoBG_%d.png", stem, i)
		}
		p := filepath.Join(outDir, name)
		// O_EXCL makes the name claim atomic, so parallel workers can never collide.
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return "", err
		}
		bw := bufio.NewWriterSize(f, 1<<20)
		werr := png.Encode(bw, img)
		if werr == nil {
			werr = bw.Flush()
		}
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

func exportOne(path, outDir, colorMode string, transparency, boldness int, autoCrop, formSize bool) (saved string, err error) {
	defer func() {
		if r := recover(); r != nil {
			saved, err = "", fmt.Errorf("Unexpected failure: %v", r)
		}
	}()
	src, err := loadCapped(path, maxSrcDim)
	if err != nil {
		return "", err
	}
	out, err := extractImage(src, colorMode, transparency, boldness, autoCrop, formSize, exportUpscale)
	if err != nil {
		return "", err
	}
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return savePNGUnique(outDir, stem, out)
}

// SaveOne exports the current photo as a 3x HD PNG into <photo folder>/PhotoBG_Output.
func (a *App) SaveOne(filePath, colorMode string, transparency, boldness int, autoCrop, formSize bool) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			msg = fmt.Sprintf("Error: %v", r)
		}
	}()
	filePath = normalizePath(filePath)
	if !isImageExt(filePath) || !fileExists(filePath) {
		return "Error: Photo not found"
	}
	outDir := filepath.Join(filepath.Dir(filePath), outDirName)
	saved, err := exportOne(filePath, outDir, colorMode, transparency, boldness, autoCrop, formSize)
	if err != nil {
		return "Error: " + err.Error()
	}
	return "Saved " + filepath.Base(saved) + " to " + outDirName
}

// ProcessBatch exports every queued photo with a bounded worker pool, then opens the folder.
func (a *App) ProcessBatch(filePaths []string, colorMode string, transparency, boldness int, autoCrop, formSize bool) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			msg = fmt.Sprintf("Error: %v", r)
		}
	}()

	valid := make([]string, 0, len(filePaths))
	seen := make(map[string]struct{}, len(filePaths))
	for _, raw := range filePaths {
		p := normalizePath(raw)
		if _, dup := seen[p]; dup || !isImageExt(p) || !fileExists(p) {
			continue
		}
		seen[p] = struct{}{}
		valid = append(valid, p)
	}
	if len(valid) == 0 {
		return "Error: No valid photos in the queue"
	}

	outDir := filepath.Join(filepath.Dir(valid[0]), outDirName)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "Error: " + err.Error()
	}

	var okCount, failCount int32
	var errMu sync.Mutex
	var firstErr error

	jobs := make(chan string)
	var wg sync.WaitGroup
	for i := 0; i < min(maxWorkers, len(valid)); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				if _, err := exportOne(p, outDir, colorMode, transparency, boldness, autoCrop, formSize); err != nil {
					atomic.AddInt32(&failCount, 1)
					errMu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("%s: %v", filepath.Base(p), err)
					}
					errMu.Unlock()
					continue
				}
				atomic.AddInt32(&okCount, 1)
			}
		}()
	}
	for _, p := range valid {
		jobs <- p
	}
	close(jobs)
	wg.Wait()

	ok, failed := int(atomic.LoadInt32(&okCount)), int(atomic.LoadInt32(&failCount))
	if ok == 0 {
		return fmt.Sprintf("Error: Could not process any photos (%v)", firstErr)
	}
	_ = openDirectory(outDir)
	if failed > 0 {
		return fmt.Sprintf("Processed %d of %d photos, %d failed (%v)", ok, len(valid), failed, firstErr)
	}
	return fmt.Sprintf("Processed %d photos to %s", ok, outDirName)
}

// ---------------------------------------------------------------------------
// Output folder and ZIP
// ---------------------------------------------------------------------------

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

func (a *App) OpenOutFolder(basePath string) {
	dir := baseDir(basePath)
	if dir == "" {
		return
	}
	outDir := filepath.Join(dir, outDirName)
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
	// PNGs are already compressed, so Store is faster and no larger.
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store, Modified: fi.ModTime()})
	if err != nil {
		return err
	}
	_, err = io.Copy(w, f)
	return err
}

// ExportZip bundles <folder>/PhotoBG_Output/*.png into <folder>/PhotoBG_Export.zip.
// The archive is written to a temp file and only moved into place once fully closed.
func (a *App) ExportZip(basePath string) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			msg = fmt.Sprintf("Error: %v", r)
		}
	}()
	dir := baseDir(basePath)
	if dir == "" {
		return "Error: Add photos first"
	}
	outDir := filepath.Join(dir, outDirName)
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return "Error: No processed images yet. Click Process All Images first"
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".png") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return "Error: No processed images yet. Click Process All Images first"
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
		// Target is locked or unwritable (e.g. open in another program): use a numbered name.
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
	return fmt.Sprintf("Exported %d images to %s", len(names), filepath.Base(final))
}
