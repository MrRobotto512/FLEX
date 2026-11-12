// Command logo draws the FLEX logo (white FLEX over a film strip on a dark
// navy field with a blue glow) and writes every icon the project ships, so
// they all come from one drawing:
//
//	go run ./tools/logo
//
// It draws with polygons and supersampling, so each size is rendered
// sharp rather than scaled down. Below 48 px the word is unreadable, so
// small icons show the F over its strip instead.
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

// The drawing, in units of a 1932-unit square (the size the logo was made at).
type pt struct{ x, y float64 }

type shape struct {
	poly  []pt       // convex polygon, or nil for a rounded rectangle
	r     [4]float64 // rounded rectangle x0, y0, x1, y1
	round float64
	col   [3]float64
	hole  bool // cut out of what is under it (the strip's sprocket holes)
}

var (
	white = [3]float64{244, 246, 249}
	strip = [3]float64{76, 155, 240}
	edge  = [3]float64{15, 18, 25}
	glow  = [3]float64{88, 172, 255}
)

func rect(x0, y0, x1, y1 float64) shape {
	return shape{poly: []pt{{x0, y0}, {x1, y0}, {x1, y1}, {x0, y1}}, col: white}
}

func quad(a, b, c, d pt) shape { return shape{poly: []pt{a, b, c, d}, col: white} }

// filmStrip is the blue bar from x0 to x1 with n sprocket holes.
func filmStrip(x0, x1 float64, n int) []shape {
	s := []shape{{r: [4]float64{x0, 1162, x1, 1222}, col: strip}}
	const w, h = 18.0, 30.0
	first, last := x0+27, x1-27-w
	for i := 0; i < n; i++ {
		x := first + (last-first)*float64(i)/float64(n-1)
		s = append(s, shape{r: [4]float64{x, 1177, x + w, 1177 + h}, round: 4, hole: true})
	}
	return s
}

var letterF = []shape{rect(276, 737, 376, 1130), rect(276, 737, 556, 818), rect(276, 908, 546, 990)}

func wordmark() []shape {
	s := append([]shape{}, letterF...)
	s = append(s,
		rect(617, 737, 717, 1130), rect(617, 1048, 895, 1130), // L
		rect(940, 737, 1040, 1130), rect(940, 737, 1240, 818), // E
		rect(940, 890, 1210, 972), rect(940, 1048, 1243, 1130),
		quad(pt{1268, 737}, pt{1393, 737}, pt{1654, 1130}, pt{1529, 1130}), // X
		quad(pt{1521, 737}, pt{1646, 737}, pt{1390, 1130}, pt{1265, 1130}),
	)
	return append(s, filmStrip(276, 1655, 28)...)
}

func compact() []shape {
	return append(append([]shape{}, letterF...), filmStrip(276, 556, 6)...)
}

func inside(s shape, p pt) bool {
	if s.poly == nil {
		r := s.r
		if p.x < r[0] || p.x > r[2] || p.y < r[1] || p.y > r[3] {
			return false
		}
		// rounded corners
		cx := math.Max(r[0]+s.round, math.Min(p.x, r[2]-s.round))
		cy := math.Max(r[1]+s.round, math.Min(p.y, r[3]-s.round))
		return (p.x-cx)*(p.x-cx)+(p.y-cy)*(p.y-cy) <= s.round*s.round+1e-9
	}
	sign := 0.0
	for i := range s.poly {
		a, b := s.poly[i], s.poly[(i+1)%len(s.poly)]
		c := (b.x-a.x)*(p.y-a.y) - (b.y-a.y)*(p.x-a.x)
		if c != 0 {
			if sign == 0 {
				sign = c
			} else if (c > 0) != (sign > 0) {
				return false
			}
		}
	}
	return true
}

type view struct {
	x0, y0, x1, y1 float64 // the part of the drawing shown
	gx, gy         float64 // where the glow is brightest
	shapes         []shape
}

func full() view {
	return view{0, 0, 1932, 1932, 930, 960, wordmark()}
}

// wide shows the word on a 16:9 field (TV banners and backgrounds).
func wide() view {
	h := 1932.0 * 9 / 16
	return view{0, 960 - h/2, 1932, 960 + h/2, 930, 960, wordmark()}
}

func small() view {
	return view{146, 720, 686, 1260, 416, 980, compact()}
}

func (v view) colorAt(p pt) [3]float64 {
	dx, dy := (p.x-v.gx)/780, (p.y-v.gy)/640
	f := math.Exp(-(dx*dx + dy*dy) * 2.6)
	var c [3]float64
	for i := range c {
		c[i] = edge[i] + (glow[i]-edge[i])*f
	}
	// the letters lift the glow around them a little
	for _, s := range v.shapes {
		if !inside(s, p) {
			continue
		}
		if s.hole {
			for i := range c {
				c[i] = c[i]*0.35 + 8
			}
			continue
		}
		c = s.col
	}
	return c
}

func render(v view, w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	ss := 4
	if w < 128 {
		ss = 8
	}
	sx, sy := (v.x1-v.x0)/float64(w), (v.y1-v.y0)/float64(h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var acc [3]float64
			for j := 0; j < ss; j++ {
				for i := 0; i < ss; i++ {
					p := pt{v.x0 + (float64(x)+(float64(i)+0.5)/float64(ss))*sx,
						v.y0 + (float64(y)+(float64(j)+0.5)/float64(ss))*sy}
					c := v.colorAt(p)
					for k := range acc {
						acc[k] += c[k]
					}
				}
			}
			n := float64(ss * ss)
			img.SetNRGBA(x, y, color.NRGBA{uint8(acc[0]/n + 0.5), uint8(acc[1]/n + 0.5), uint8(acc[2]/n + 0.5), 255})
		}
	}
	return img
}

func icon(size int) *image.NRGBA {
	if size < 48 {
		return render(small(), size, size)
	}
	return render(full(), size, size)
}

func pngBytes(img image.Image) []byte {
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		panic(err)
	}
	return b.Bytes()
}

func write(path string, data []byte) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		panic(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("%-55s %7d bytes\n", path, len(data))
}

// ico packs PNG images into a Windows .ico (PNG entries, Vista and later).
func ico(sizes ...int) []byte {
	var imgs [][]byte
	for _, s := range sizes {
		imgs = append(imgs, pngBytes(icon(s)))
	}
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, [3]uint16{0, 1, uint16(len(sizes))})
	off := 6 + 16*len(sizes)
	for i, s := range sizes {
		d := uint8(s)
		if s >= 256 {
			d = 0
		}
		b.Write([]byte{d, d, 0, 0})
		binary.Write(&b, binary.LittleEndian, [2]uint16{1, 32})
		binary.Write(&b, binary.LittleEndian, [2]uint32{uint32(len(imgs[i])), uint32(off)})
		off += len(imgs[i])
	}
	for _, d := range imgs {
		b.Write(d)
	}
	return b.Bytes()
}

func main() {
	write("assets/logo/flex-1024.png", pngBytes(icon(1024)))
	write("assets/logo/flex-wide-1920.png", pngBytes(render(wide(), 1920, 1080)))
	write("ps5-client/sce_sys/icon0.png", pngBytes(icon(512)))
	write("ps5-client/tile/app/sce_sys/icon0.png", pngBytes(icon(512)))
	for dir, s := range map[string]int{"mdpi": 48, "hdpi": 72, "xhdpi": 96, "xxhdpi": 144, "xxxhdpi": 192} {
		write("android/app/src/main/res/mipmap-"+dir+"/ic_launcher.png", pngBytes(icon(s)))
	}
	write("android/app/src/main/res/drawable/banner.png", pngBytes(render(wide(), 320, 180)))
	write("desktop/tray.ico", ico(16, 20, 24, 32, 48, 64))
	write("desktop/appicon.ico", ico(16, 20, 24, 32, 40, 48, 64, 128, 256))
	write("web/favicon.png", pngBytes(icon(64)))
}
