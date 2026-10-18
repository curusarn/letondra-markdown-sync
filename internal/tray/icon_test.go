package tray

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"testing"
)

func TestIcon(t *testing.T) {
	for _, s := range []string{"ok", "syncing", "conflict", "offline", "error", "unknown"} {
		ico := Icon(s)
		var hdr [3]uint16
		binary.Read(bytes.NewReader(ico[:6]), binary.LittleEndian, &hdr)
		if hdr != [3]uint16{0, 1, 1} {
			t.Fatalf("%s: bad ICO header %v", s, hdr)
		}
		img, err := png.Decode(bytes.NewReader(ico[22:]))
		if err != nil || img.Bounds().Dx() != 32 {
			t.Fatalf("%s: bad PNG: %v", s, err)
		}
	}
}
