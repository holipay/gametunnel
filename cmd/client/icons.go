package main

import (
	"bytes"
	"encoding/binary"
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

// generateIcon creates a 16x16 ICO icon with the given accent color.
// The ICO wraps a PNG image (supported since Windows Vista).
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
	for x := 5; x < 11; x++ {
		img.Set(x, 8, white)
	}
	img.Set(9, 7, white)
	img.Set(9, 9, white)
	img.Set(10, 6, white)
	img.Set(10, 10, white)

	// Encode as PNG
	var pngBuf bytes.Buffer
	png.Encode(&pngBuf, img)
	pngData := pngBuf.Bytes()

	// Wrap PNG in ICO container
	// ICO header: reserved(2) + type(2) + count(2)
	// ICO directory entry: 16 bytes
	// Followed by the PNG data
	var buf bytes.Buffer

	// Header
	binary.Write(&buf, binary.LittleEndian, uint16(0)) // reserved
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // type: ICO
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // image count

	// Directory entry
	const headerSize = 6
	const dirEntrySize = 16
	dataOffset := uint32(headerSize + dirEntrySize)

	buf.WriteByte(16)                    // width
	buf.WriteByte(16)                    // height
	buf.WriteByte(0)                     // color palette
	buf.WriteByte(0)                     // reserved
	binary.Write(&buf, binary.LittleEndian, uint16(1))  // color planes
	binary.Write(&buf, binary.LittleEndian, uint16(32)) // bits per pixel
	binary.Write(&buf, binary.LittleEndian, uint32(len(pngData))) // data size
	binary.Write(&buf, binary.LittleEndian, dataOffset)          // data offset

	// PNG image data
	buf.Write(pngData)

	return buf.Bytes()
}
