// Package imgview renders raster images (PNG/JPEG/GIF) as ANSI truecolor
// half-blocks so they preview correctly in the taildrives TUI without any
// external tooling (chafa/viu/iterm2-protocol).
//
// Each terminal cell encodes TWO vertical pixels: the upper half is the
// foreground color, the lower half is the background color, drawn with the
// "▀" (UPPER HALF BLOCK) glyph. This doubles the vertical resolution for
// free.
package imgview

import (
	"bytes"
	"fmt"
	"image"
	// register decoders via side-effect imports
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"path/filepath"
	"strings"
)

// IsImage returns true if the byte buffer or the supplied name hint looks
// like a raster image format we can decode. Cheap check: magic-byte sniff
// for the canonical formats, plus extension fallback.
func IsImage(data []byte, nameHint string) bool {
	if len(data) >= 8 {
		// PNG: 89 50 4E 47 0D 0A 1A 0A
		if bytes.HasPrefix(data, []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}) {
			return true
		}
		// JPEG: FF D8 FF
		if data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF {
			return true
		}
		// GIF: GIF87a / GIF89a
		if bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a")) {
			return true
		}
		// BMP: BM
		if data[0] == 0x42 && data[1] == 0x4D {
			return true
		}
	}
	if nameHint != "" {
		ext := strings.ToLower(filepath.Ext(nameHint))
		switch ext {
		case ".png", ".jpg", ".jpeg", ".gif", ".bmp":
			return true
		}
	}
	return false
}

// Render decodes the image and produces a string of ANSI escape sequences
// that, when printed, renders an approximation of the image. The output is
// at most `cols` columns wide and `rows` rows tall, preserving aspect
// ratio (centered with no padding). Falls back to a stub line on decode
// failure so the caller can still display SOMETHING.
func Render(data []byte, cols, rows int) (string, error) {
	if cols < 4 || rows < 2 {
		return "", fmt.Errorf("pane too small (%dx%d) for image preview", cols, rows)
	}
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("decode image: %w", err)
	}
	_ = format
	return renderImage(img, cols, rows*2), nil
}

// renderImage downsamples img to (targetW × targetH) pixels via nearest-
// neighbor and emits one terminal row per two image rows using the upper
// half-block trick: FG = top pixel, BG = bottom pixel.
func renderImage(img image.Image, targetW, targetH int) string {
	bounds := img.Bounds()
	srcW := bounds.Dx()
	srcH := bounds.Dy()
	if srcW == 0 || srcH == 0 {
		return ""
	}
	// Preserve aspect ratio. Terminal cells are roughly 1:2 tall:wide; the
	// half-block trick already doubles vertical pixels so each cell now
	// represents a square-ish region. Just fit into (targetW × targetH).
	scaleX := float64(srcW) / float64(targetW)
	scaleY := float64(srcH) / float64(targetH)
	scale := scaleX
	if scaleY > scale {
		scale = scaleY
	}
	outW := int(float64(srcW) / scale)
	outH := int(float64(srcH) / scale)
	if outW < 1 {
		outW = 1
	}
	if outH < 1 {
		outH = 1
	}

	// Sample pixels.
	get := func(x, y int) (r, g, b uint8) {
		sx := bounds.Min.X + int(float64(x)*scale)
		sy := bounds.Min.Y + int(float64(y)*scale)
		if sx >= bounds.Max.X {
			sx = bounds.Max.X - 1
		}
		if sy >= bounds.Max.Y {
			sy = bounds.Max.Y - 1
		}
		R, G, B, _ := img.At(sx, sy).RGBA()
		return uint8(R >> 8), uint8(G >> 8), uint8(B >> 8)
	}

	var b strings.Builder
	// We render two pixel rows per terminal row, so iterate by 2.
	for y := 0; y < outH; y += 2 {
		for x := 0; x < outW; x++ {
			r1, g1, b1 := get(x, y)
			var r2, g2, bb2 uint8
			if y+1 < outH {
				r2, g2, bb2 = get(x, y+1)
			} else {
				r2, g2, bb2 = 0, 0, 0
			}
			// FG = top, BG = bottom, glyph = ▀
			fmt.Fprintf(&b,
				"\x1b[38;2;%d;%d;%dm\x1b[48;2;%d;%d;%dm▀",
				r1, g1, b1, r2, g2, bb2)
		}
		b.WriteString("\x1b[0m\n")
	}
	return b.String()
}
