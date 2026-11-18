package events

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png"
	"math"
	"strings"
)

func validRawBBox(box []float64) bool {
	if len(box) != 4 {
		return false
	}
	for _, v := range box {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	x, y, w, h := box[0], box[1], box[2], box[3]
	if w <= 0 || h <= 0 {
		return false
	}
	return x < 1 && y < 1 && x+w > 0 && y+h > 0
}

// ShouldAnnotateBBox reports whether ev carries at least one valid normalized
// [x, y, w, h] bounding box that should be rendered onto the saved detection snapshot.
func ShouldAnnotateBBox(ev Event) bool {
	if validRawBBox(ev.BBox) {
		return true
	}
	for _, d := range ev.Detections {
		if validRawBBox(d.BBox) {
			return true
		}
	}
	return false
}

// AnnotateDetectionJPEG decodes imgBytes (JPEG or PNG), draws a Frigate-style
// bounding box and label for ev when ShouldAnnotateBBox(ev) is true, and
// returns the re-encoded JPEG bytes. If ev has no valid bounding box, imgBytes
// is returned unchanged.
func AnnotateDetectionJPEG(imgBytes []byte, ev Event) ([]byte, error) {
	if !ShouldAnnotateBBox(ev) {
		return imgBytes, nil
	}
	src, _, err := image.Decode(bytes.NewReader(imgBytes))
	if err != nil {
		return nil, err
	}
	b := src.Bounds()
	rgba := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(rgba, rgba.Bounds(), src, b.Min, draw.Src)

	DrawDetectionBox(rgba, ev)

	var out bytes.Buffer
	if err := jpeg.Encode(&out, rgba, &jpeg.Options{Quality: 88}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// DrawDetectionBox draws Frigate-style bounding boxes and label pills onto dst
// for every valid detection in ev.Detections (falling back to ev.BBox).
func DrawDetectionBox(dst draw.Image, ev Event) {
	if dst == nil || !ShouldAnnotateBBox(ev) {
		return
	}
	var targets []Event
	for _, d := range ev.Detections {
		if validRawBBox(d.BBox) {
			targets = append(targets, Event{
				Type:       d.Type,
				Class:      d.Class,
				Confidence: d.Confidence,
				BBox:       d.BBox,
			})
		}
	}
	if len(targets) == 0 && validRawBBox(ev.BBox) {
		targets = []Event{ev}
	}
	for _, item := range targets {
		drawSingleDetectionBox(dst, item)
	}
}

func drawSingleDetectionBox(dst draw.Image, ev Event) {
	if !validRawBBox(ev.BBox) {
		return
	}
	b := dst.Bounds()
	imgW, imgH := b.Dx(), b.Dy()
	if imgW < 8 || imgH < 8 {
		return
	}

	x1 := clampInt(int(math.Round(ev.BBox[0]*float64(imgW))), 0, imgW-1)
	y1 := clampInt(int(math.Round(ev.BBox[1]*float64(imgH))), 0, imgH-1)
	x2 := clampInt(int(math.Round((ev.BBox[0]+ev.BBox[2])*float64(imgW))), x1+1, imgW)
	y2 := clampInt(int(math.Round((ev.BBox[1]+ev.BBox[3])*float64(imgH))), y1+1, imgH)

	// Guarantee a visible box even for very small normalized dimensions.
	const minBox = 4
	if x2-x1 < minBox {
		x2 = min(imgW, x1+minBox)
		if x2-x1 < minBox {
			x1 = max(0, x2-minBox)
		}
	}
	if y2-y1 < minBox {
		y2 = min(imgH, y1+minBox)
		if y2-y1 < minBox {
			y1 = max(0, y2-minBox)
		}
	}

	col := detectionColor(ev.Type)
	colImg := image.NewUniform(col)
	t := max(2, imgW/320)

	// Top, bottom, left, right edges of the bounding box.
	fillRect(dst, b.Min.X+x1, b.Min.Y+y1, b.Min.X+x2, b.Min.Y+min(imgH, y1+t), colImg)
	fillRect(dst, b.Min.X+x1, b.Min.Y+max(0, y2-t), b.Min.X+x2, b.Min.Y+y2, colImg)
	fillRect(dst, b.Min.X+x1, b.Min.Y+y1, b.Min.X+min(imgW, x1+t), b.Min.Y+y2, colImg)
	fillRect(dst, b.Min.X+max(0, x2-t), b.Min.Y+y1, b.Min.X+x2, b.Min.Y+y2, colImg)

	label := detectionLabel(ev)
	if label == "" {
		return
	}
	scale := max(2, imgW/400)
	runes := []rune(label)
	adv := 6 * scale
	glyphH := 7 * scale
	padX := 3 * scale
	padY := 2 * scale
	textW := len(runes)*adv - scale
	labelW := textW + 2*padX
	labelH := glyphH + 2*padY

	lx1 := x1
	if lx1+labelW > imgW {
		lx1 = max(0, imgW-labelW)
	}
	// Frigate places the label bar just above the top edge when space allows,
	// falling back inside the top-left corner when the box touches the top.
	ly1 := y1 - labelH
	if ly1 < 0 {
		ly1 = y1
	}
	lx2 := min(imgW, lx1+labelW)
	ly2 := min(imgH, ly1+labelH)

	fillRect(dst, b.Min.X+lx1, b.Min.Y+ly1, b.Min.X+lx2, b.Min.Y+ly2, colImg)
	drawBitmapText(dst, b.Min.X+lx1+padX, b.Min.Y+ly1+padY, runes, scale, color.RGBA{255, 255, 255, 255})
}

func fillRect(dst draw.Image, x1, y1, x2, y2 int, src image.Image) {
	if x2 <= x1 || y2 <= y1 {
		return
	}
	draw.Draw(dst, image.Rect(x1, y1, x2, y2), src, image.Point{}, draw.Src)
}

func detectionColor(evType string) color.RGBA {
	switch strings.ToLower(strings.TrimSpace(evType)) {
	case "person":
		return color.RGBA{59, 130, 246, 255} // #3b82f6 blue
	case "vehicle":
		return color.RGBA{16, 185, 129, 255} // #10b981 emerald green
	case "lpr":
		return color.RGBA{139, 92, 246, 255} // #8b5cf6 purple
	case "face":
		return color.RGBA{236, 72, 153, 255} // #ec4899 pink
	case "line_crossing":
		return color.RGBA{249, 115, 22, 255} // #f97316 orange
	case "intrusion":
		return color.RGBA{239, 68, 68, 255} // #ef4444 red
	case "loitering":
		return color.RGBA{234, 88, 12, 255} // #ea580c deep orange
	case "motion":
		return color.RGBA{245, 158, 11, 255} // #f59e0b amber
	default:
		return color.RGBA{59, 130, 246, 255}
	}
}

func detectionLabel(ev Event) string {
	t := strings.ToLower(strings.TrimSpace(ev.Type))
	cls := strings.TrimSpace(ev.Class)
	lowCls := strings.ToLower(cls)
	var base string
	switch t {
	case "person":
		if cls != "" && lowCls != "person" && lowCls != "human" && lowCls != "persona" && lowCls != "pedestrian" {
			base = cls
		} else {
			base = "Persona"
		}
	case "vehicle":
		switch lowCls {
		case "", "vehicle", "vehiculo", "vehículo", "vehice":
			base = "Vehículo"
		case "car", "auto", "sedan":
			base = "Auto"
		case "motorcycle", "motorbike", "motor", "moto":
			base = "Moto"
		case "bicycle", "bike", "bicicleta", "tricycle":
			base = "Bicicleta"
		case "truck", "camion":
			base = "Camión"
		case "bus", "colectivo":
			base = "Bus"
		case "suv":
			base = "SUV"
		case "van":
			base = "Van"
		default:
			base = cls
		}
	case "lpr":
		if cls != "" {
			base = cls
		} else {
			base = "LPR"
		}
	case "face":
		if cls != "" && !strings.EqualFold(cls, "face") {
			base = cls
		} else {
			base = "Rostro"
		}
	case "motion":
		if cls != "" && !strings.EqualFold(cls, "motion") {
			base = cls
		} else {
			base = "Movimiento"
		}
	case "intrusion":
		base = "Perimetro"
	case "line_crossing":
		base = "Cruce linea"
	default:
		if cls != "" {
			base = cls
		} else {
			base = ev.Type
		}
	}
	if ev.Confidence != nil && *ev.Confidence > 0 {
		pct := clampInt(int(math.Round(*ev.Confidence*100)), 1, 100)
		base = fmt.Sprintf("%s %d%%", base, pct)
	}
	return base
}

func drawBitmapText(dst draw.Image, x, y int, runes []rune, scale int, fg color.RGBA) {
	b := dst.Bounds()
	adv := 6 * scale
	for i, r := range runes {
		g, ok := bitmapFont5x7[r]
		if !ok {
			g = bitmapFont5x7['?']
		}
		gx := x + i*adv
		for row := range 7 {
			bits := g[row]
			for col := range 5 {
				if (bits>>(4-col))&1 == 0 {
					continue
				}
				px1 := gx + col*scale
				py1 := y + row*scale
				for dy := range scale {
					py := py1 + dy
					if py < b.Min.Y || py >= b.Max.Y {
						continue
					}
					for dx := range scale {
						px := px1 + dx
						if px < b.Min.X || px >= b.Max.X {
							continue
						}
						dst.Set(px, py, fg)
					}
				}
			}
		}
	}
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// bitmapFont5x7 is a minimal 5x7 crisp pixel font (5 bits per row, 7 rows).
var bitmapFont5x7 = map[rune][7]uint8{
	' ': {0b00000, 0b00000, 0b00000, 0b00000, 0b00000, 0b00000, 0b00000},
	'%': {0b11001, 0b11010, 0b00010, 0b00100, 0b01000, 0b01011, 0b10011},
	'(': {0b00010, 0b00100, 0b01000, 0b01000, 0b01000, 0b00100, 0b00010},
	')': {0b01000, 0b00100, 0b00010, 0b00010, 0b00010, 0b00100, 0b01000},
	',': {0b00000, 0b00000, 0b00000, 0b00000, 0b00110, 0b00100, 0b01000},
	'-': {0b00000, 0b00000, 0b00000, 0b11111, 0b00000, 0b00000, 0b00000},
	'.': {0b00000, 0b00000, 0b00000, 0b00000, 0b00000, 0b01100, 0b01100},
	'/': {0b00001, 0b00010, 0b00010, 0b00100, 0b01000, 0b01000, 0b10000},
	':': {0b00000, 0b01100, 0b01100, 0b00000, 0b01100, 0b01100, 0b00000},
	'?': {0b01110, 0b10001, 0b00001, 0b00110, 0b00100, 0b00000, 0b00100},
	'_': {0b00000, 0b00000, 0b00000, 0b00000, 0b00000, 0b00000, 0b11111},
	'·': {0b00000, 0b00000, 0b00000, 0b00100, 0b00000, 0b00000, 0b00000},

	'0': {0b01110, 0b10001, 0b10011, 0b10101, 0b11001, 0b10001, 0b01110},
	'1': {0b00100, 0b01100, 0b00100, 0b00100, 0b00100, 0b00100, 0b01110},
	'2': {0b01110, 0b10001, 0b00001, 0b00110, 0b01000, 0b10000, 0b11111},
	'3': {0b11110, 0b00001, 0b00001, 0b01110, 0b00001, 0b00001, 0b11110},
	'4': {0b00010, 0b00110, 0b01010, 0b10010, 0b11111, 0b00010, 0b00010},
	'5': {0b11111, 0b10000, 0b11110, 0b00001, 0b00001, 0b10001, 0b01110},
	'6': {0b00110, 0b01000, 0b10000, 0b11110, 0b10001, 0b10001, 0b01110},
	'7': {0b11111, 0b00001, 0b00010, 0b00100, 0b01000, 0b01000, 0b01000},
	'8': {0b01110, 0b10001, 0b10001, 0b01110, 0b10001, 0b10001, 0b01110},
	'9': {0b01110, 0b10001, 0b10001, 0b01111, 0b00001, 0b00010, 0b01100},

	'A': {0b01110, 0b10001, 0b10001, 0b11111, 0b10001, 0b10001, 0b10001},
	'B': {0b11110, 0b10001, 0b10001, 0b11110, 0b10001, 0b10001, 0b11110},
	'C': {0b01110, 0b10001, 0b10000, 0b10000, 0b10000, 0b10001, 0b01110},
	'D': {0b11110, 0b10001, 0b10001, 0b10001, 0b10001, 0b10001, 0b11110},
	'E': {0b11111, 0b10000, 0b10000, 0b11110, 0b10000, 0b10000, 0b11111},
	'F': {0b11111, 0b10000, 0b10000, 0b11110, 0b10000, 0b10000, 0b10000},
	'G': {0b01110, 0b10001, 0b10000, 0b10111, 0b10001, 0b10001, 0b01110},
	'H': {0b10001, 0b10001, 0b10001, 0b11111, 0b10001, 0b10001, 0b10001},
	'I': {0b01110, 0b00100, 0b00100, 0b00100, 0b00100, 0b00100, 0b01110},
	'J': {0b00111, 0b00010, 0b00010, 0b00010, 0b10010, 0b10010, 0b01100},
	'K': {0b10001, 0b10010, 0b10100, 0b11000, 0b10100, 0b10010, 0b10001},
	'L': {0b10000, 0b10000, 0b10000, 0b10000, 0b10000, 0b10000, 0b11111},
	'M': {0b10001, 0b11011, 0b10101, 0b10101, 0b10001, 0b10001, 0b10001},
	'N': {0b10001, 0b11001, 0b10101, 0b10011, 0b10001, 0b10001, 0b10001},
	'O': {0b01110, 0b10001, 0b10001, 0b10001, 0b10001, 0b10001, 0b01110},
	'P': {0b11110, 0b10001, 0b10001, 0b11110, 0b10000, 0b10000, 0b10000},
	'Q': {0b01110, 0b10001, 0b10001, 0b10001, 0b10101, 0b10010, 0b01101},
	'R': {0b11110, 0b10001, 0b10001, 0b11110, 0b10100, 0b10010, 0b10001},
	'S': {0b01111, 0b10000, 0b10000, 0b01110, 0b00001, 0b00001, 0b11110},
	'T': {0b11111, 0b00100, 0b00100, 0b00100, 0b00100, 0b00100, 0b00100},
	'U': {0b10001, 0b10001, 0b10001, 0b10001, 0b10001, 0b10001, 0b01110},
	'V': {0b10001, 0b10001, 0b10001, 0b10001, 0b10001, 0b01010, 0b00100},
	'W': {0b10001, 0b10001, 0b10001, 0b10101, 0b10101, 0b11011, 0b10001},
	'X': {0b10001, 0b10001, 0b01010, 0b00100, 0b01010, 0b10001, 0b10001},
	'Y': {0b10001, 0b10001, 0b01010, 0b00100, 0b00100, 0b00100, 0b00100},
	'Z': {0b11111, 0b00001, 0b00010, 0b00100, 0b01000, 0b10000, 0b11111},

	'a': {0b00000, 0b00000, 0b01110, 0b00001, 0b01111, 0b10001, 0b01111},
	'b': {0b10000, 0b10000, 0b10110, 0b11001, 0b10001, 0b10001, 0b11110},
	'c': {0b00000, 0b00000, 0b01110, 0b10001, 0b10000, 0b10001, 0b01110},
	'd': {0b00001, 0b00001, 0b01101, 0b10011, 0b10001, 0b10001, 0b01111},
	'e': {0b00000, 0b00000, 0b01110, 0b10001, 0b11111, 0b10000, 0b01110},
	'f': {0b00110, 0b01001, 0b01000, 0b11100, 0b01000, 0b01000, 0b01000},
	'g': {0b00000, 0b01111, 0b10001, 0b10001, 0b01111, 0b00001, 0b01110},
	'h': {0b10000, 0b10000, 0b10110, 0b11001, 0b10001, 0b10001, 0b10001},
	'i': {0b00100, 0b00000, 0b01100, 0b00100, 0b00100, 0b00100, 0b01110},
	'j': {0b00010, 0b00000, 0b00110, 0b00010, 0b00010, 0b10010, 0b01100},
	'k': {0b10000, 0b10000, 0b10010, 0b10100, 0b11000, 0b10100, 0b10010},
	'l': {0b01100, 0b00100, 0b00100, 0b00100, 0b00100, 0b00100, 0b01110},
	'm': {0b00000, 0b00000, 0b11010, 0b10101, 0b10101, 0b10001, 0b10001},
	'n': {0b00000, 0b00000, 0b10110, 0b11001, 0b10001, 0b10001, 0b10001},
	'o': {0b00000, 0b00000, 0b01110, 0b10001, 0b10001, 0b10001, 0b01110},
	'p': {0b00000, 0b00000, 0b11110, 0b10001, 0b11110, 0b10000, 0b10000},
	'q': {0b00000, 0b00000, 0b01101, 0b10011, 0b01111, 0b00001, 0b00001},
	'r': {0b00000, 0b00000, 0b10110, 0b11001, 0b10000, 0b10000, 0b10000},
	's': {0b00000, 0b00000, 0b01111, 0b10000, 0b01110, 0b00001, 0b11110},
	't': {0b01000, 0b01000, 0b11100, 0b01000, 0b01000, 0b01001, 0b00110},
	'u': {0b00000, 0b00000, 0b10001, 0b10001, 0b10001, 0b10011, 0b01101},
	'v': {0b00000, 0b00000, 0b10001, 0b10001, 0b10001, 0b01010, 0b00100},
	'w': {0b00000, 0b00000, 0b10001, 0b10001, 0b10101, 0b10101, 0b01010},
	'x': {0b00000, 0b00000, 0b10001, 0b01010, 0b00100, 0b01010, 0b10001},
	'y': {0b00000, 0b00000, 0b10001, 0b10001, 0b01111, 0b00001, 0b01110},
	'z': {0b00000, 0b00000, 0b11111, 0b00010, 0b00100, 0b01000, 0b11111},

	'á': {0b00010, 0b00100, 0b01110, 0b00001, 0b01111, 0b10001, 0b01111},
	'é': {0b00010, 0b00100, 0b01110, 0b10001, 0b11111, 0b10000, 0b01110},
	'í': {0b00010, 0b00100, 0b00000, 0b01100, 0b00100, 0b00100, 0b01110},
	'ó': {0b00010, 0b00100, 0b01110, 0b10001, 0b10001, 0b10001, 0b01110},
	'ú': {0b00010, 0b00100, 0b10001, 0b10001, 0b10001, 0b10011, 0b01101},
	'ñ': {0b01101, 0b10010, 0b00000, 0b10110, 0b11001, 0b10001, 0b10001},
	'Á': {0b00010, 0b00100, 0b01110, 0b10001, 0b11111, 0b10001, 0b10001},
	'É': {0b00010, 0b00100, 0b11111, 0b10000, 0b11110, 0b10000, 0b11111},
	'Í': {0b00010, 0b00100, 0b01110, 0b00100, 0b00100, 0b00100, 0b01110},
	'Ó': {0b00010, 0b00100, 0b01110, 0b10001, 0b10001, 0b10001, 0b01110},
	'Ú': {0b00010, 0b00100, 0b10001, 0b10001, 0b10001, 0b10001, 0b01110},
	'Ñ': {0b01101, 0b10010, 0b10001, 0b11001, 0b10101, 0b10011, 0b10001},
}
