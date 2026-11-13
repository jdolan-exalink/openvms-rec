package main

import (
	"bytes"
	"encoding/binary"
	"math"
)

// Tray colours per state.
var (
	colorOK      = [3]byte{0x2d, 0xa4, 0x4e} // green: recording everything
	colorWarning = [3]byte{0xd2, 0x99, 0x22} // amber: something needs attention
	colorDown    = [3]byte{0xcf, 0x22, 0x2e} // red: recorder not answering
	colorStopped = [3]byte{0x8c, 0x95, 0x9f} // grey: service stopped on purpose
)

// iconICO draws a 32×32 disc of rgb with a dark ring and a white centre dot
// ("recording" light) as a Windows .ico.
func iconICO(rgb [3]byte) []byte {
	const n = 32
	px := make([]byte, n*n*4) // BGRA, bottom-up
	c := float64(n-1) / 2
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			d := math.Hypot(float64(x)-c, float64(y)-c)
			var r, g, b, a byte
			switch {
			case d <= 5:
				r, g, b, a = 0xff, 0xff, 0xff, 0xff
			case d <= 13:
				r, g, b, a = rgb[0], rgb[1], rgb[2], 0xff
			case d <= 15:
				r, g, b, a = rgb[0]/2, rgb[1]/2, rgb[2]/2, 0xff
			case d <= 15.8:
				r, g, b, a = rgb[0]/2, rgb[1]/2, rgb[2]/2, byte(255*(15.8-d)/0.8)
			}
			i := ((n-1-y)*n + x) * 4
			px[i], px[i+1], px[i+2], px[i+3] = b, g, r, a
		}
	}
	mask := make([]byte, n*4) // 1 bpp AND mask, rows padded to 32 bits; all opaque (alpha decides)
	var img bytes.Buffer
	binary.Write(&img, binary.LittleEndian, struct {
		Size                         uint32
		Width, Height                int32
		Planes, BitCount             uint16
		Compression, SizeImage       uint32
		XPPM, YPPM, ClrUsed, ClrImpt int32
	}{40, n, n * 2, 1, 32, 0, uint32(len(px) + len(mask)), 0, 0, 0, 0})
	img.Write(px)
	img.Write(mask)

	var out bytes.Buffer
	binary.Write(&out, binary.LittleEndian, [3]uint16{0, 1, 1}) // reserved, type icon, count
	binary.Write(&out, binary.LittleEndian, struct {
		W, H, Colors, Reserved byte
		Planes, BitCount       uint16
		Size, Offset           uint32
	}{n, n, 0, 0, 1, 32, uint32(img.Len()), 6 + 16})
	out.Write(img.Bytes())
	return out.Bytes()
}
