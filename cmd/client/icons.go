package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
)

// iconDisconnected is a gray tunnel icon.
var iconDisconnected = generateIcon(color.RGBA{R: 128, G: 128, B: 128, A: 255})

// iconConnected is a green tunnel icon.
var iconConnected = generateIcon(color.RGBA{R: 0, G: 200, B: 83, A: 255})

// iconConnecting is a yellow tunnel icon.
var iconConnecting = generateIcon(color.RGBA{R: 255, G: 193, B: 7, A: 255})

// generateIcon creates a 16x16 PNG icon with the given color.
// Draws a simple tunnel/pill shape.
func generateIcon(c color.RGBA) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))

	// Background: transparent
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			img.Set(x, y, color.RGBA{0, 0, 0, 0})
		}
	}

	// Draw a rounded rectangle (tunnel shape)
	for y := 2; y < 14; y++ {
		for x := 2; x < 14; x++ {
			// Rounded corners
			if (x < 4 && y < 4) || (x > 11 && y < 4) || (x < 4 && y > 11) || (x > 11 && y > 11) {
				dx, dy := 0, 0
				if x < 4 {
					dx = 3 - x
				} else {
					dx = x - 12
				}
				if y < 4 {
					dy = 3 - y
				} else {
					dy = y - 12
				}
				if dx*dx+dy*dy > 4 {
					continue
				}
			}
			img.Set(x, y, c)
		}
	}

	// Draw a white arrow/chevron in the center
	white := color.RGBA{255, 255, 255, 255}
	// Horizontal line
	for x := 5; x < 11; x++ {
		img.Set(x, 8, white)
	}
	// Arrow head
	img.Set(9, 7, white)
	img.Set(9, 9, white)
	img.Set(10, 6, white)
	img.Set(10, 10, white)

	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}
