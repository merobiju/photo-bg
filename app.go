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

	thumbMax      = 64
	previewMax    = 900
	maxSrcDim     = 1200
	exportUpscale = 3
	maxWorkers    = 4
	maxDropImages = 500

	maxDecodePixels  = 120_000_000
	maxProcessPixels = 16_000_000

	bgRadius = 64
	cropPad  = 6
	formW    = 600
	formH    = 200

	previewCacheMax = 12
)

var imageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".bmp": true, ".webp": true, ".tif": true, ".tiff": true,
}

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
	img  *image.NRGBA
	orig string
}

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

func (a *App) OnStartup(ctx context.Context) {
	a.ctx = ctx
}

func (a *App) OnDomReady(ctx context.Context) {
	if a.ctx == nil {
		a.ctx = ctx
	}
	a.dropOnce.Do(func() {
		wailsrt.OnFileDrop(ctx, func(x, y int, paths []string) {
			defer func() { _ = recover() }()
			resolved := collectImages(paths, maxDropImages)
			wailsrt.EventsEmit(a.ctx, "files-dropped", resolved)
		})
	})
}

func (a *App) GetDefaultFolder() string {
	return ""
}

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

func (a *App) ResolvePath(inputPath string) []string {
	return collectImages([]string{inputPath}, 0)
}

func isImageExt(p string) bool {
	return imageExts[strings.ToLower(filepath.Ext(p))]
}

func cleanInput(s string) string {
	s = strings.TrimSpace(s)
	for len(s) >= 2 {
		first := s[0]
		last := s[len(s)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			s = strings.TrimSpace(s[1 : len(s)-1])
			continue
		}
		break
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

func collectImages(paths []string, limit int) []string {
	out := make([]string, 0, len(paths))
	seen := make(map[string]struct{})
	add := func(p string) bool {
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
	return &image.NRGBA{Pix: dst.Pix, Stride: dst.Stride, Rect: dst.Rect}
}

func opaqueSource(img image.Image) image.Image {
	if isOpaque(img) {
		return img
	}
	return flatten(img)
}

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
func smooth3x3Safe(src *image.NRGBA) []uint8 {
	w, h := src.Rect.Dx(), src.Rect.Dy()
	pix, stride := src.Pix, src.Stride
	tmp := make([]uint8, w*h*3)
	out := make([]uint8, w*h*3)

	for y := 0; y < h; y++ {
		row := pix[y*stride : y*stride+w*4]
		dstRow := tmp[y*w*3 : (y+1)*w*3]
		for x := 0; x < w; x++ {
			xl := max(0, x-1) * 4
			xc := x * 4
			xr := min(w-1, x+1) * 4
			di := x * 3
			dstRow[di] = uint8((uint32(row[xl]) + 2*uint32(row[xc]) + uint32(row[xr]) + 2) >> 2)
			dstRow[di+1] = uint8((uint32(row[xl+1]) + 2*uint32(row[xc+1]) + uint32(row[xr+1]) + 2) >> 2)
			dstRow[di+2] = uint8((uint32(row[xl+2]) + 2*uint32(row[xc+2]) + uint32(row[xr+2]) + 2) >> 2)
		}
	}

	rowBytes := w * 3
	for y := 0; y < h; y++ {
		yt := max(0, y-1) * rowBytes
		yc := y * rowBytes
		yb := min(h-1, y+1) * rowBytes
		dstRow := out[yc : yc+rowBytes]
		for i := 0; i < rowBytes; i++ {
			dstRow[i] = uint8((uint32(tmp[yt+i]) + 2*uint32(tmp[yc+i]) + uint32(tmp[yb+i]) + 2) >> 2)
		}
	}
	return out
}

func extractImage(src image.Image, colorMode string, transparency, boldness int, autoCrop, formSize bool, upscale int) (*image.NRGBA, error) {
	img, _, err := extractWithCrop(src, colorMode, transparency, boldness, autoCrop, formSize, upscale)
	return img, err
}

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

	smoothed := smooth3x3Safe(img)

	sw := w + 1
	satR := make([]uint64, sw*(h+1))
	satG := make([]uint64, sw*(h+1))
	satB := make([]uint64, sw*(h+1))
	for y := 0; y < h; y++ {
		var rowR, rowG, rowB uint64
		rowOff := y * w * 3
		above := y * sw
		cur := (y + 1) * sw
		for x := 0; x < w; x++ {
			si := rowOff + x*3
			rowR += uint64(smoothed[si])
			rowG += uint64(smoothed[si+1])
			rowB += uint64(smoothed[si+2])
			satR[cur+x+1] = satR[above+x+1] + rowR
			satG[cur+x+1] = satG[above+x+1] + rowG
			satB[cur+x+1] = satB[above+x+1] + rowB
		}
	}

	tf := float64(transparency) / 100.0
	bf := float64(boldness) / 100.0

	ceilRatio := 0.988 - tf*0.088
	solidRatio := 0.725 + bf*0.180
	span := ceilRatio - solidRatio
	if span < 0.004 {
		span = 0.004
	}
	minAlpha := min(0.90, 0.03+tf*0.22)
	gamma := 1.15 - bf*0.55

	const lutMax = 4095
	var alphaLUT [lutMax + 1]uint8
	for i := 0; i <= lutMax; i++ {
		ratio := float64(i) / float64(lutMax)
		if ratio >= ceilRatio {
			continue
		}
		a := clampFloat((ceilRatio-ratio)/span, 0.0, 1.0)
		a = math.Pow(a, gamma)
		if a <= minAlpha {
			continue
		}
		a = clampFloat((a-minAlpha)/(1.0-minAlpha), 0.0, 1.0)
		alphaLUT[i] = uint8(min(255, int(a*255.0+0.5)))
	}
	var cr, cg, cb uint8
	useOrig := true
	switch colorMode {
	case "black":
		useOrig = false
	case "blue":
		useOrig, cr, cg, cb = false, 15, 55, 195
	}

	out := image.NewNRGBA(full)
	op := out.Pix
	pix, stride := img.Pix, img.Stride
	minX, minY, maxX, maxY := w, h, -1, -1

	for y := 0; y < h; y++ {
		y0 := max(0, y-bgRadius)
		y1 := min(h, y+bgRadius+1)
		rowY0 := y0 * sw
		rowY1 := y1 * sw
		smoothRow := y * w * 3
		srcRow := y * stride
		outRow := y * out.Stride

		for x := 0; x < w; x++ {
			x0 := max(0, x-bgRadius)
			x1 := min(w, x+bgRadius+1)
			area := float32((x1 - x0) * (y1 - y0))

			idx11 := rowY1 + x1
			idx01 := rowY0 + x1
			idx10 := rowY1 + x0
			idx00 := rowY0 + x0

			bgR := max(float32(1.0), float32(satR[idx11]-satR[idx01]-satR[idx10]+satR[idx00])/area)
			bgG := max(float32(1.0), float32(satG[idx11]-satG[idx01]-satG[idx10]+satG[idx00])/area)
			bgB := max(float32(1.0), float32(satB[idx11]-satB[idx01]-satB[idx10]+satB[idx00])/area)

			si := smoothRow + x*3
			minRatio := min(float32(smoothed[si])/bgR, min(float32(smoothed[si+1])/bgG, float32(smoothed[si+2])/bgB))

			lutIdx := clampInt(int(minRatio*float32(lutMax)+0.5), 0, lutMax)
			ai := alphaLUT[lutIdx]
			if ai == 0 {
				continue
			}

			o := outRow + x*4
			if useOrig {
				s := srcRow + x*4
				if ai == 255 {
					op[o], op[o+1], op[o+2] = pix[s], pix[s+1], pix[s+2]
				} else {
					a := float32(ai) / 255.0
					invA := 1.0 - a
					recipA := 1.0 / a
					op[o] = uint8(clampFloat32((float32(pix[s])-bgR*invA)*recipA, 0, 255) + 0.5)
					op[o+1] = uint8(clampFloat32((float32(pix[s+1])-bgG*invA)*recipA, 0, 255) + 0.5)
					op[o+2] = uint8(clampFloat32((float32(pix[s+2])-bgB*invA)*recipA, 0, 255) + 0.5)
				}
			} else {
				op[o], op[o+1], op[o+2] = cr, cg, cb
			}
			op[o+3] = ai

			minX, maxX = min(minX, x), max(maxX, x)
			minY, maxY = min(minY, y), max(maxY, y)
		}
	}

	if maxX < 0 {
		if formSize {
			return image.NewNRGBA(image.Rect(0, 0, formW, formH)), full, nil
		}
		return image.NewNRGBA(image.Rect(0, 0, w, h)), full, nil
	}

	crop := full
	res := out
	if autoCrop || formSize {
		crop = image.Rect(minX-cropPad, minY-cropPad, maxX+1+cropPad, maxY+1+cropPad).Intersect(full)
		res = cropNRGBA(out, crop)
	}

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

func clampFloat(v, lo, hi float64) float64 {
	return max(lo, min(hi, v))
}

func clampFloat32(v, lo, hi float32) float32 {
	return max(lo, min(hi, v))
}
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
			for _, oldestKey := range a.order {
				delete(a.cache, oldestKey)
				break
			}
			a.order = a.order[1:]
		}
	}
	a.mu.Unlock()
	return ps, nil
}

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

	var firstPath string
	for _, p := range valid {
		firstPath = p
		break
	}
	outDir := filepath.Join(filepath.Dir(firstPath), outDirName)
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
	go func() { _ = cmd.Wait() }()
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
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store, Modified: fi.ModTime()})
	if err != nil {
		return err
	}
	_, err = io.Copy(w, f)
	return err
}

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
