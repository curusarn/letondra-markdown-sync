// Package tray shows the app in the Windows notification area.
package tray

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
)

// Colors per state.
var stateColor = map[string]color.RGBA{
	"ok":       {0x1a, 0x7f, 0x37, 0xff},
	"syncing":  {0x0b, 0x57, 0xd0, 0xff},
	"conflict": {0xe6, 0x7e, 0x00, 0xff},
	"offline":  {0x80, 0x80, 0x80, 0xff},
	"error":    {0xc6, 0x28, 0x28, 0xff},
}

// Icon returns a 32×32 .ico (PNG inside) with a coloured disc and a white
// page mark, coloured by state.
func Icon(state string) []byte {
	c, ok := stateColor[state]
	if !ok {
		c = stateColor["ok"]
	}
	const n = 32
	img := image.NewRGBA(image.Rect(0, 0, n, n))
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			d := math.Hypot(float64(x)-15.5, float64(y)-15.5)
			switch {
			case d <= 15:
				img.Set(x, y, c)
				// a white "page" in the middle
				if x >= 10 && x <= 21 && y >= 8 && y <= 23 && !(x >= 18 && y <= 11 && x-18 > y-8) {
					img.Set(x, y, color.RGBA{0xff, 0xff, 0xff, 0xff})
					if (y == 13 || y == 16 || y == 19) && x >= 12 && x <= 19 {
						img.Set(x, y, c)
					}
				}
			case d <= 16:
				a := uint8(255 * (16 - d))
				img.Set(x, y, color.RGBA{c.R, c.G, c.B, a})
			}
		}
	}
	var pngBuf bytes.Buffer
	png.Encode(&pngBuf, img)
	var ico bytes.Buffer
	binary.Write(&ico, binary.LittleEndian, []uint16{0, 1, 1})
	ico.Write([]byte{n, n, 0, 0})
	binary.Write(&ico, binary.LittleEndian, []uint16{1, 32})
	binary.Write(&ico, binary.LittleEndian, []uint32{uint32(pngBuf.Len()), 22})
	ico.Write(pngBuf.Bytes())
	return ico.Bytes()
}
