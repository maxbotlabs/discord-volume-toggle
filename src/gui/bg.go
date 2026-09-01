// Background pixel decoding for the pre-rendered DIB section.
package gui

import (
	"bytes"
	"image/png"

	"discord-volume-toggle/src/background"
)

// decodeBackgroundPixels decodes the embedded pre-composited background PNG
// into raw RGBA pixels. Returns (0,0,nil) on failure (background disabled).
func decodeBackgroundPixels() (w, h int, pixels []byte) {
	img, err := png.Decode(bytes.NewReader(background.PNG))
	if err != nil {
		return 0, 0, nil
	}
	b := img.Bounds()
	w = b.Dx()
	h = b.Dy()
	pixels = make([]byte, w*h*4)
	idx := 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, al := img.At(x, y).RGBA()
			pixels[idx+0] = byte(r >> 8)
			pixels[idx+1] = byte(g >> 8)
			pixels[idx+2] = byte(bl >> 8)
			pixels[idx+3] = byte(al >> 8)
			idx += 4
		}
	}
	return w, h, pixels
}