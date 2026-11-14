package muse

import (
	"bytes"
	"context"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const (
	// compactSingleBytes is the per-image raw size above which reference
	// images are re-encoded as JPEG before embedding into /chat/stream.
	compactSingleBytes = 512 << 10 // 512 KiB

	// compactTotalBytes caps the combined raw byte size of all reference
	// images in a single turn so the Base64-encoded JSON body stays well
	// within the Hatch gateway's 256-chunk Noise frame assembly limit.
	compactTotalBytes = 3 << 20 // 3 MiB raw (~4 MiB Base64, ~86 chunks of 48 KiB)

	compactMinBudget = 128 << 10 // 128 KiB floor per image
)

type compactStep struct {
	maxDim  int
	quality int
}

var compactSteps = []compactStep{
	{maxDim: 1920, quality: 85},
	{maxDim: 1600, quality: 80},
	{maxDim: 1280, quality: 75},
	{maxDim: 1024, quality: 70},
	{maxDim: 768, quality: 65},
	{maxDim: 640, quality: 55},
}

// CompactRefImages normalizes and compresses large reference images so that
// multi-image or lossless PNG inputs do not exceed the upstream Hatch gateway
// message assembly limit when Base64-encoded into /chat/stream.
func CompactRefImages(refs []RefImage) []RefImage {
	if len(refs) == 0 {
		return nil
	}
	total := 0
	needsCompact := false
	for _, r := range refs {
		total += len(r.Data)
		if len(r.Data) > compactSingleBytes {
			needsCompact = true
		}
	}
	if !needsCompact && total <= compactTotalBytes {
		return refs
	}

	budget := max(min(compactSingleBytes, compactTotalBytes/len(refs)), compactMinBudget)
	out := make([]RefImage, len(refs))
	for i, r := range refs {
		out[i] = compactOneRefImage(r, budget)
	}
	return out
}

func compactOneRefImage(r RefImage, budget int) RefImage {
	if len(r.Data) <= budget {
		return r
	}
	src, _, err := image.Decode(bytes.NewReader(r.Data))
	if err != nil {
		if transcoded, ok := transcodeWebPWithFFmpeg(r.Data, budget); ok {
			return transcoded
		}
		return r
	}
	b := src.Bounds()
	srcW, srcH := b.Dx(), b.Dy()
	if srcW <= 0 || srcH <= 0 {
		return r
	}

	best := r
	var buf bytes.Buffer
	var lastW, lastH int
	var cached *image.RGBA

	for _, step := range compactSteps {
		dstW, dstH := fitDimensions(srcW, srcH, step.maxDim)
		if cached == nil || dstW != lastW || dstH != lastH {
			cached = resizeOpaque(src, dstW, dstH)
			lastW, lastH = dstW, dstH
		}
		buf.Reset()
		if err := jpeg.Encode(&buf, cached, &jpeg.Options{Quality: step.quality}); err != nil {
			continue
		}
		if buf.Len() < len(best.Data) {
			best = RefImage{Data: bytes.Clone(buf.Bytes()), Mime: "image/jpeg"}
		}
		if buf.Len() <= budget {
			return best
		}
	}
	return best
}

func fitDimensions(w, h, maxDim int) (int, int) {
	if w <= maxDim && h <= maxDim {
		return w, h
	}
	if w >= h {
		return maxDim, max(1, (h*maxDim+w/2)/w)
	}
	return max(1, (w*maxDim+h/2)/h), maxDim
}

// resizeOpaque renders src into an opaque RGBA image of size dstW x dstH,
// compositing any transparent pixels over a white background.
func resizeOpaque(src image.Image, dstW, dstH int) *image.RGBA {
	b := src.Bounds()
	srcW, srcH := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))

	switch s := src.(type) {
	case *image.NRGBA:
		for y := 0; y < dstH; y++ {
			sy := b.Min.Y + (y*srcH)/dstH
			srcRow := (sy - s.Rect.Min.Y) * s.Stride
			dstRow := y * dst.Stride
			for x := 0; x < dstW; x++ {
				sx := b.Min.X + (x*srcW)/dstW
				si := srcRow + (sx-s.Rect.Min.X)*4
				r := uint32(s.Pix[si])
				g := uint32(s.Pix[si+1])
				bl := uint32(s.Pix[si+2])
				a := uint32(s.Pix[si+3])
				di := dstRow + x*4
				if a == 255 {
					dst.Pix[di] = uint8(r)
					dst.Pix[di+1] = uint8(g)
					dst.Pix[di+2] = uint8(bl)
				} else if a == 0 {
					dst.Pix[di] = 255
					dst.Pix[di+1] = 255
					dst.Pix[di+2] = 255
				} else {
					inv := 255 - a
					dst.Pix[di] = uint8((r*a + 255*inv + 127) / 255)
					dst.Pix[di+1] = uint8((g*a + 255*inv + 127) / 255)
					dst.Pix[di+2] = uint8((bl*a + 255*inv + 127) / 255)
				}
				dst.Pix[di+3] = 255
			}
		}
	case *image.RGBA:
		for y := 0; y < dstH; y++ {
			sy := b.Min.Y + (y*srcH)/dstH
			srcRow := (sy - s.Rect.Min.Y) * s.Stride
			dstRow := y * dst.Stride
			for x := 0; x < dstW; x++ {
				sx := b.Min.X + (x*srcW)/dstW
				si := srcRow + (sx-s.Rect.Min.X)*4
				r := uint32(s.Pix[si])
				g := uint32(s.Pix[si+1])
				bl := uint32(s.Pix[si+2])
				a := uint32(s.Pix[si+3])
				di := dstRow + x*4
				if a == 255 {
					dst.Pix[di] = uint8(r)
					dst.Pix[di+1] = uint8(g)
					dst.Pix[di+2] = uint8(bl)
				} else {
					inv := 255 - a
					dst.Pix[di] = uint8(min(r+inv, 255))
					dst.Pix[di+1] = uint8(min(g+inv, 255))
					dst.Pix[di+2] = uint8(min(bl+inv, 255))
				}
				dst.Pix[di+3] = 255
			}
		}
	default:
		for y := 0; y < dstH; y++ {
			sy := b.Min.Y + (y*srcH)/dstH
			dstRow := y * dst.Stride
			for x := 0; x < dstW; x++ {
				sx := b.Min.X + (x*srcW)/dstW
				r16, g16, b16, a16 := src.At(sx, sy).RGBA()
				inv := uint32(0xffff - a16)
				di := dstRow + x*4
				dst.Pix[di] = uint8(min((r16+inv)>>8, 255))
				dst.Pix[di+1] = uint8(min((g16+inv)>>8, 255))
				dst.Pix[di+2] = uint8(min((b16+inv)>>8, 255))
				dst.Pix[di+3] = 255
			}
		}
	}
	return dst
}

// transcodeWebPWithFFmpeg converts large WebP images to JPEG using local ffmpeg
// when available, since the Go standard library does not include a WebP decoder.
func transcodeWebPWithFFmpeg(data []byte, budget int) (RefImage, bool) {
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return RefImage{}, false
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return RefImage{}, false
	}
	dir, err := os.MkdirTemp("", "muse-webp-*")
	if err != nil {
		return RefImage{}, false
	}
	defer os.RemoveAll(dir)
	inPath := filepath.Join(dir, "in.webp")
	outPath := filepath.Join(dir, "out.jpg")
	if err := os.WriteFile(inPath, data, 0o600); err != nil {
		return RefImage{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := runFFmpeg(ctx, ffmpeg, "-i", inPath, "-vf", "scale='min(1920,iw)':'min(1920,ih)':force_original_aspect_ratio=decrease", "-frames:v", "1", "-q:v", "5", outPath); err != nil {
		return RefImage{}, false
	}
	jpg, err := os.ReadFile(outPath)
	if err != nil || len(jpg) == 0 || len(jpg) >= len(data) {
		return RefImage{}, false
	}
	ref := RefImage{Data: jpg, Mime: "image/jpeg"}
	if len(jpg) > budget {
		return compactOneRefImage(ref, budget), true
	}
	return ref, true
}
