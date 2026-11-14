package muse

import (
	"bytes"
	"image"
	"image/png"
	"testing"

	"github.com/CoolkHz/muse-protocol-go/internal/fakemuse"
)

func makeLargeTestPNG(t *testing.T, w, h, seed int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	// Deterministic pseudo-random detailed pattern so PNG is multi-megabyte
	// while still compressing cleanly as JPEG.
	s := uint32(seed*1664525 + 1013904223)
	for y := 0; y < h; y++ {
		row := y * img.Stride
		for x := 0; x < w; x++ {
			s = s*1664525 + 1013904223
			i := row + x*4
			img.Pix[i] = uint8((x*255)/w) ^ uint8(s>>24)&0x3f
			img.Pix[i+1] = uint8((y*255)/h) ^ uint8(s>>16)&0x3f
			img.Pix[i+2] = uint8(((x+y)*255)/(w+h)) ^ uint8(s>>8)&0x3f
			img.Pix[i+3] = 255
		}
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, img); err != nil {
		t.Fatalf("encode test png: %v", err)
	}
	return buf.Bytes()
}

func TestCompactRefImagesLeavesSmallImagesUntouched(t *testing.T) {
	small := []RefImage{{Data: fakemuse.PNG(), Mime: "image/png"}}
	got := CompactRefImages(small)
	if len(got) != 1 || got[0].Mime != "image/png" || !bytes.Equal(got[0].Data, small[0].Data) {
		t.Fatalf("small image was modified: mime=%s len=%d", got[0].Mime, len(got[0].Data))
	}
}

func TestCompactRefImagesCompressesFive2KPNGsWithinGatewayLimit(t *testing.T) {
	pngData := makeLargeTestPNG(t, 2048, 1152, 1)
	if len(pngData) <= compactSingleBytes {
		t.Fatalf("expected large test PNG > %d bytes, got %d", compactSingleBytes, len(pngData))
	}
	refs := make([]RefImage, 5)
	rawTotal := 0
	for i := range refs {
		refs[i] = RefImage{Data: pngData, Mime: "image/png"}
		rawTotal += len(pngData)
	}

	got := CompactRefImages(refs)
	if len(got) != 5 {
		t.Fatalf("len(got) = %d, want 5", len(got))
	}
	compactedTotal := 0
	for i, r := range got {
		if r.Mime != "image/jpeg" {
			t.Fatalf("ref[%d] mime = %q, want image/jpeg", i, r.Mime)
		}
		if len(r.Data) > compactSingleBytes {
			t.Fatalf("ref[%d] size = %d, exceeds single budget %d", i, len(r.Data), compactSingleBytes)
		}
		cfg, format, err := image.DecodeConfig(bytes.NewReader(r.Data))
		if err != nil || format != "jpeg" {
			t.Fatalf("ref[%d] decode config: format=%q err=%v", i, format, err)
		}
		if cfg.Width > 1920 || cfg.Height > 1920 {
			t.Fatalf("ref[%d] dimensions = %dx%d, want <= 1920", i, cfg.Width, cfg.Height)
		}
		compactedTotal += len(r.Data)
	}
	if compactedTotal > compactTotalBytes {
		t.Fatalf("compacted total = %d bytes (from %d), exceeds %d", compactedTotal, rawTotal, compactTotalBytes)
	}
}

func TestCompactRefImagesHandlesTenMaxImages(t *testing.T) {
	pngData := makeLargeTestPNG(t, 1920, 1080, 2)
	refs := make([]RefImage, MaxRefImages)
	for i := range refs {
		refs[i] = RefImage{Data: pngData, Mime: "image/png"}
	}
	got := CompactRefImages(refs)
	total := 0
	for _, r := range got {
		total += len(r.Data)
	}
	if total > compactTotalBytes {
		t.Fatalf("10 compacted images total = %d, want <= %d", total, compactTotalBytes)
	}
}
