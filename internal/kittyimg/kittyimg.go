// Package kittyimg implements the Kitty terminal graphics protocol — used
// by Ghostty, Kitty, and WezTerm to render real raster images inline in a
// terminal. Replaces the imgview ANSI-half-block approach (~80x80 visible
// pixels at typical pane sizes, basically unrecognizable for actual photos)
// with pixel-perfect inline rendering at full pane resolution.
//
// Protocol reference: https://sw.kovidgoyal.net/kitty/graphics-protocol/
//
// Wire summary:
//   - Each image is encoded as a base64 PNG payload
//   - Payload is chunked at 4096-byte boundaries (protocol max chunk size)
//   - Each chunk is wrapped in APC envelope: ESC _ G key=val[,…] ; data ESC \
//   - The first chunk carries the metadata (action, format, columns, rows);
//     middle and last chunks just carry m=1 / m=0 (more / no more)
//   - On non-Kitty terminals the sequences are silently swallowed (or
//     dumped as garbage) — callers MUST gate with Supported() first.
package kittyimg

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"os"
	"strings"

	"golang.org/x/image/draw"
)

// Supported returns true when the current terminal advertises Kitty graphics
// protocol support via known environment variables. Conservative: defaults
// to false. Adding a new known-good terminal is one case-line below.
//
// The reliable detection method is a terminal-capability query (DA1/DCS),
// but doing that synchronously requires raw-mode tty access which is fiddly
// from inside Bubble Tea. Env-based detection covers the common case.
func Supported() bool {
	term := os.Getenv("TERM")
	termProg := os.Getenv("TERM_PROGRAM")
	switch {
	case strings.Contains(term, "kitty"):
		return true
	case strings.Contains(term, "ghostty"):
		return true
	case strings.Contains(term, "wezterm"):
		return true
	case termProg == "WezTerm":
		return true
	case termProg == "ghostty":
		return true
	case os.Getenv("KITTY_WINDOW_ID") != "":
		return true
	case os.Getenv("GHOSTTY_RESOURCES_DIR") != "":
		return true
	}
	return false
}

// EncodePNG resizes an image to fit inside (maxCols × maxRows) terminal
// cells at the supplied cell-pixel ratio, then PNG-encodes it. Returns the
// raw PNG bytes plus the actual (cols, rows) the image will occupy when
// placed — caller uses these to pad the layout so other content doesn't
// overlap. cellW/cellH are pixels-per-cell; common Ghostty default is
// ~9×18 but the protocol places by cell count so exact values aren't
// critical, just the aspect ratio.
func EncodePNG(img image.Image, maxCols, maxRows, cellW, cellH int) ([]byte, int, int, error) {
	if maxCols < 4 || maxRows < 2 {
		return nil, 0, 0, fmt.Errorf("pane too small (%dx%d)", maxCols, maxRows)
	}
	if cellW <= 0 {
		cellW = 9
	}
	if cellH <= 0 {
		cellH = 18
	}

	bounds := img.Bounds()
	srcW, srcH := bounds.Dx(), bounds.Dy()
	if srcW == 0 || srcH == 0 {
		return nil, 0, 0, fmt.Errorf("empty image")
	}

	// Pixel budget = (maxCols * cellW) × (maxRows * cellH).
	maxPxW := maxCols * cellW
	maxPxH := maxRows * cellH

	// Preserve aspect ratio.
	scaleX := float64(maxPxW) / float64(srcW)
	scaleY := float64(maxPxH) / float64(srcH)
	scale := scaleX
	if scaleY < scale {
		scale = scaleY
	}
	if scale > 1 {
		scale = 1 // never upscale — looks blurry
	}

	dstW := int(float64(srcW) * scale)
	dstH := int(float64(srcH) * scale)
	if dstW < 1 {
		dstW = 1
	}
	if dstH < 1 {
		dstH = 1
	}

	// CatmullRom is good quality with reasonable cost for ~thumbnail sizes.
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, bounds, draw.Over, nil)

	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return nil, 0, 0, fmt.Errorf("png encode: %w", err)
	}

	// Translate pixel size back to cell count for layout reservation.
	occupiedCols := (dstW + cellW - 1) / cellW
	occupiedRows := (dstH + cellH - 1) / cellH
	if occupiedCols > maxCols {
		occupiedCols = maxCols
	}
	if occupiedRows > maxRows {
		occupiedRows = maxRows
	}

	return buf.Bytes(), occupiedCols, occupiedRows, nil
}

// Sequence builds the chunked APC escape sequence that draws the supplied
// PNG bytes at the current cursor position, sized to (cols × rows) cells.
// The image is drawn ephemerally (a=T) — re-emit on each frame.
//
// Why ephemeral instead of upload-once-place-many (a=t / a=p): the Bubble
// Tea render loop produces a fresh View string per frame, so persisting
// state across frames means managing image IDs + delete commands. For
// thumbnail-sized images (sub-100KB PNG after resize) the transmission
// overhead is negligible.
func Sequence(pngData []byte, cols, rows int) string {
	const chunkSize = 4096 // protocol-defined max base64 chunk

	enc := base64.StdEncoding.EncodeToString(pngData)
	var b strings.Builder

	for i := 0; i < len(enc); i += chunkSize {
		end := i + chunkSize
		if end > len(enc) {
			end = len(enc)
		}
		chunk := enc[i:end]
		more := 1
		if end == len(enc) {
			more = 0
		}

		b.WriteString("\x1b_G")
		if i == 0 {
			// First chunk carries the action + dimensions metadata.
			fmt.Fprintf(&b, "a=T,f=100,c=%d,r=%d,q=2,m=%d;", cols, rows, more)
		} else {
			fmt.Fprintf(&b, "m=%d;", more)
		}
		b.WriteString(chunk)
		b.WriteString("\x1b\\")
	}

	return b.String()
}

// DeleteAll emits the Kitty sequence that clears all images from the
// terminal. Useful when switching previews or closing the preview pane —
// otherwise stale images can linger as ghost layers on top of new content.
func DeleteAll() string {
	return "\x1b_Ga=d,d=A,q=2;\x1b\\"
}

// IsImage returns true for the formats Go's stdlib image decoder can
// natively handle. The kittyimg pipeline decode→resize→re-encode produces
// PNG regardless of input format.
func IsImage(data []byte, nameHint string) bool {
	if len(data) >= 8 {
		if bytes.HasPrefix(data, []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}) {
			return true // PNG
		}
		if data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF {
			return true // JPEG
		}
		if bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a")) {
			return true
		}
	}
	if nameHint != "" {
		lower := strings.ToLower(nameHint)
		for _, ext := range []string{".png", ".jpg", ".jpeg", ".gif", ".webp"} {
			if strings.HasSuffix(lower, ext) {
				return true
			}
		}
	}
	return false
}

// DecodeAndEncode is a one-shot convenience: takes raw image bytes, decodes
// via the stdlib (PNG/JPEG/GIF; WEBP would need a 3rd-party decoder),
// resizes to fit the supplied cell box, and returns the encoded PNG plus
// the cells it will occupy. Wraps EncodePNG + image.Decode for the common
// preview pipeline.
func DecodeAndEncode(raw []byte, maxCols, maxRows, cellW, cellH int) ([]byte, int, int, error) {
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("decode: %w", err)
	}
	return EncodePNG(img, maxCols, maxRows, cellW, cellH)
}
