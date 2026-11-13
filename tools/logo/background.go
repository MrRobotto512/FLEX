package main

// The PS5 home screen shows two 3840x2160 pictures of an app's own: one
// behind the menu while its tile is selected (pic0) and one while it starts
// (pic1). These draw both from the logo. Positions are in pixels of the 4K
// picture; smaller renders (previews) scale them down.

import (
	"image"
	"image/color"
	"math"
	"runtime"
	"sync"
)

const bgW, bgH = 3840.0, 2160.0

type rgb = [3]float64

var screenBG = rgb{15, 17, 21} // FLEX's own background on the PS5 (C_BG)

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }

func smooth(e0, e1, x float64) float64 {
	t := clamp01((x - e0) / (e1 - e0))
	return t * t * (3 - 2*t)
}

func mix(a, b rgb, t float64) rgb {
	for i := range a {
		a[i] += (b[i] - a[i]) * t
	}
	return a
}

func scale(a rgb, f float64) rgb {
	for i := range a {
		a[i] *= f
	}
	return a
}

// light adds a coloured light of strength t.
func light(a, b rgb, t float64) rgb {
	for i := range a {
		a[i] += b[i] * t
	}
	return a
}

func blob(x, y, cx, cy, rx, ry float64) float64 {
	dx, dy := (x-cx)/rx, (y-cy)/ry
	return math.Exp(-(dx*dx + dy*dy))
}

// boxSDF is the signed distance from (x, y) to a rounded box centred on the
// origin with half sizes hx, hy: negative inside.
func boxSDF(x, y, hx, hy, r float64) float64 {
	qx, qy := math.Abs(x)-hx+r, math.Abs(y)-hy+r
	return math.Hypot(math.Max(qx, 0), math.Max(qy, 0)) + math.Min(math.Max(qx, qy), 0) - r
}

func hash(v uint32) uint32 {
	v ^= v >> 16
	v *= 0x7feb352d
	v ^= v >> 15
	v *= 0x846ca68b
	v ^= v >> 16
	return v
}

func unit(v uint32) float64 { return float64(hash(v)) / 4294967296 }

// canvas is a picture in floating point, so glows add up without banding
// until the very end.
type canvas struct {
	w, h int
	px   []rgb
}

// paint calls f for every pixel with the pixel's centre in 4K units and the
// width of one pixel in those units (edges are drawn a pixel soft).
func (c *canvas) paint(f func(x, y, px float64, p *rgb)) {
	rows := make(chan int, c.h)
	for y := 0; y < c.h; y++ {
		rows <- y
	}
	close(rows)
	px := bgW / float64(c.w)
	var wg sync.WaitGroup
	for i := 0; i < runtime.NumCPU(); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for y := range rows {
				for x := 0; x < c.w; x++ {
					f((float64(x)+0.5)*px, (float64(y)+0.5)*px, px, &c.px[y*c.w+x])
				}
			}
		}()
	}
	wg.Wait()
}

// image rounds the canvas to 8 bits with a little grain, which also keeps the
// dark gradients from banding on a TV.
func (c *canvas) image(grain float64) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, c.w, c.h))
	for y := 0; y < c.h; y++ {
		for x := 0; x < c.w; x++ {
			n := uint32(y*c.w + x)
			g := (unit(n*2+0x9e37) + unit(n*2+0x79b9) - 1) * grain
			p := c.px[y*c.w+x]
			var v [3]uint8
			for i := range p {
				v[i] = uint8(math.Max(0, math.Min(255, math.Floor(p[i]+g+0.5))))
			}
			img.SetNRGBA(x, y, color.NRGBA{v[0], v[1], v[2], 255})
		}
	}
	return img
}

// placed is the wordmark drawn with its centre at (cx, cy), w wide.
type placed struct {
	cx, cy, w float64
	shapes    []shape
}

const logoX0, logoX1, logoY0, logoY1 = 276.0, 1655.0, 737.0, 1222.0

func (l placed) unit() float64 { return l.w / (logoX1 - logoX0) }

func (l placed) toLogo(x, y float64) pt {
	u := l.unit()
	return pt{(x-l.cx)/u + (logoX0+logoX1)/2, (y-l.cy)/u + (logoY0+logoY1)/2}
}

// solid is 1 where the logo is (for its halo).
func (l placed) solid(x, y float64) float64 {
	p := l.toLogo(x, y)
	for _, s := range l.shapes {
		if !s.hole && inside(s, p) {
			return 1
		}
	}
	return 0
}

// draw puts the logo over the colour under it, 16 samples a pixel.
func (l placed) draw(x, y, px float64, under rgb) rgb {
	u := l.unit()
	hw, hh := (logoX1-logoX0)/2*u, (logoY1-logoY0)/2*u
	if math.Abs(x-l.cx) > hw+px || math.Abs(y-l.cy) > hh+px {
		return under
	}
	const n = 4
	var acc rgb
	for j := 0; j < n; j++ {
		for i := 0; i < n; i++ {
			p := l.toLogo(x+((float64(i)+0.5)/n-0.5)*px, y+((float64(j)+0.5)/n-0.5)*px)
			c := under
			for _, s := range l.shapes {
				if !inside(s, p) {
					continue
				}
				if s.hole {
					c = light(scale(c, 0.35), rgb{8, 8, 8}, 1)
					continue
				}
				c = s.col
			}
			acc = light(acc, c, 1)
		}
	}
	return scale(acc, 1.0/(n*n))
}

// field is a blurred mask on a coarse grid (a glow around a shape).
type field struct {
	w, h int
	cell float64
	v    []float64
}

func blurred(cell float64, radius int, cover func(x, y float64) float64) *field {
	f := &field{w: int(bgW / cell), h: int(bgH / cell), cell: cell}
	f.v = make([]float64, f.w*f.h)
	for j := 0; j < f.h; j++ {
		for i := 0; i < f.w; i++ {
			var a float64
			for _, o := range [4]pt{{0.25, 0.25}, {0.75, 0.25}, {0.25, 0.75}, {0.75, 0.75}} {
				a += cover((float64(i)+o.x)*cell, (float64(j)+o.y)*cell)
			}
			f.v[j*f.w+i] = a / 4
		}
	}
	for pass := 0; pass < 3; pass++ {
		f.v = boxBlur(f.v, f.w, f.h, radius, 1, 0)
		f.v = boxBlur(f.v, f.w, f.h, radius, 0, 1)
	}
	return f
}

func boxBlur(v []float64, w, h, r, dx, dy int) []float64 {
	out := make([]float64, len(v))
	for j := 0; j < h; j++ {
		for i := 0; i < w; i++ {
			var sum float64
			for k := -r; k <= r; k++ {
				x, y := i+k*dx, j+k*dy
				if x >= 0 && y >= 0 && x < w && y < h {
					sum += v[y*w+x]
				}
			}
			out[j*w+i] = sum / float64(2*r+1)
		}
	}
	return out
}

func (f *field) at(x, y float64) float64 {
	gx, gy := x/f.cell-0.5, y/f.cell-0.5
	x0, y0 := int(math.Floor(gx)), int(math.Floor(gy))
	tx, ty := gx-float64(x0), gy-float64(y0)
	get := func(i, j int) float64 {
		if i < 0 || j < 0 || i >= f.w || j >= f.h {
			return 0
		}
		return f.v[j*f.w+i]
	}
	return (get(x0, y0)*(1-tx)+get(x0+1, y0)*tx)*(1-ty) + (get(x0, y0+1)*(1-tx)+get(x0+1, y0+1)*tx)*ty
}

// film is a strip of 35 mm film laid along an arc: sprocket holes along both
// edges and a little picture in every frame.
type film struct {
	c        pt      // centre of the arc's circle
	r        float64 // its radius
	a0       float64 // angle where the strip starts (s = 0)
	dir, up  float64 // which way s grows; which side of the strip is up
	hw       float64 // half the strip's width
	fadeIn   float64 // s by which it has faded in
	alpha    float64
	firstPic int // which pictures it shows
}

// filmThrough lays a strip of half-width hw along the arc through a, b and c,
// starting at a.
func filmThrough(a, b, c pt, hw float64) film {
	d := 2 * (a.x*(b.y-c.y) + b.x*(c.y-a.y) + c.x*(a.y-b.y))
	sa, sb, sc := a.x*a.x+a.y*a.y, b.x*b.x+b.y*b.y, c.x*c.x+c.y*c.y
	o := pt{(sa*(b.y-c.y) + sb*(c.y-a.y) + sc*(a.y-b.y)) / d, (sa*(c.x-b.x) + sb*(a.x-c.x) + sc*(b.x-a.x)) / d}
	f := film{c: o, r: math.Hypot(a.x-o.x, a.y-o.y), a0: math.Atan2(a.y-o.y, a.x-o.x), hw: hw, alpha: 1, fadeIn: hw * 6}
	f.dir = 1
	if math.Remainder(math.Atan2(c.y-o.y, c.x-o.x)-f.a0, 2*math.Pi) < 0 {
		f.dir = -1
	}
	f.up = -1 // outward from the centre is down when the centre is above
	if b.y < o.y {
		f.up = 1
	}
	return f
}

func (f film) at(x, y, px float64, under rgb) rgb {
	dx, dy := x-f.c.x, y-f.c.y
	d := math.Hypot(dx, dy) - f.r
	ad := math.Abs(d)
	shadow := f.hw * 0.6
	if ad > f.hw+shadow {
		return under
	}
	s := f.dir * math.Remainder(math.Atan2(dy, dx)-f.a0, 2*math.Pi) * f.r
	a := f.alpha * smooth(0, f.fadeIn, s)
	if a <= 0 {
		return under
	}
	out := scale(under, 1-0.55*a*smooth(f.hw+shadow, f.hw*0.9, ad))
	band := clamp01(0.5 - (ad-f.hw)/px)
	if band <= 0 {
		return out
	}
	hw := f.hw
	col := rgb{11, 13, 18}
	// a faint sheen along both edges
	e := (ad - hw*0.955) / (hw * 0.018)
	col = light(col, rgb{70, 90, 120}, 0.5*math.Exp(-e*e))
	// sprocket holes, four to a frame, showing the light behind
	pitch := hw * 0.45
	hs := math.Mod(s, pitch)
	if hs < 0 {
		hs += pitch
	}
	hole := clamp01(0.5 - boxSDF(hs-pitch/2, ad-hw*0.78, hw*0.115, hw*0.075, hw*0.035)/px)
	col = mix(col, light(under, glow, 0.10), hole)
	// the frames
	frameLen := 4 * pitch
	n := math.Floor(s / frameLen)
	fs := s - n*frameLen - frameLen/2
	hl, hh := frameLen/2-hw*0.05, hw*0.58
	if frame := clamp01(0.5 - boxSDF(fs, d, hl, hh, hw*0.03)/px); frame > 0 {
		u, v := (fs+hl)/(2*hl), (f.up*d+hh)/(2*hh)
		col = mix(col, picture(f.firstPic+int(n), u, v, px/(2*hl), px/(2*hh), hl/hh), frame)
	}
	return mix(out, col, band*a)
}

// A frame's picture: sky, a sun or moon, two rows of hills. Frames come in
// shots of three, the sun moving a little from one to the next.
type palette struct{ top, horizon, sun, far, near rgb }

var palettes = []palette{
	{rgb{28, 36, 92}, rgb{238, 136, 88}, rgb{255, 220, 160}, rgb{58, 34, 72}, rgb{24, 16, 38}},      // dusk
	{rgb{6, 12, 38}, rgb{36, 74, 150}, rgb{226, 236, 255}, rgb{14, 22, 48}, rgb{6, 10, 24}},         // night
	{rgb{64, 136, 228}, rgb{182, 222, 255}, rgb{255, 250, 232}, rgb{40, 104, 136}, rgb{18, 56, 84}}, // day
	{rgb{18, 58, 92}, rgb{120, 208, 208}, rgb{255, 242, 204}, rgb{18, 70, 80}, rgb{8, 36, 46}},      // dawn
	{rgb{40, 22, 84}, rgb{206, 96, 182}, rgb{255, 206, 232}, rgb{56, 24, 78}, rgb{24, 10, 40}},      // violet
}

func picture(n int, u, v, pu, pv, aspect float64) rgb {
	shot := int(math.Floor(float64(n) / 3))
	k := uint32(shot)*16 + 7
	p := palettes[(shot*3+int(hash(k)%2))%len(palettes)] // never two shots alike in a row
	c := mix(p.horizon, p.top, smooth(0.2, 1, v))
	sx := 0.2 + 0.5*unit(k+1) + 0.05*float64(n-shot*3)
	sy, sr := 0.5+0.25*unit(k+2), 0.08+0.05*unit(k+3)
	ds := math.Hypot((u-sx)*aspect, v-sy)
	c = mix(c, p.sun, 0.35*math.Exp(-ds*ds/(sr*sr*5)))
	c = mix(c, p.sun, clamp01(0.5-(ds-sr)/pv))
	ph := 6.28 * unit(k+4)
	far := 0.40 + 0.07*math.Sin(u*6+ph) + 0.03*math.Sin(u*17+ph*2)
	c = mix(c, p.far, clamp01(0.5-(v-far)/pv))
	near := 0.22 + 0.09*math.Sin(u*3.5+ph*3) + 0.025*math.Sin(u*21+ph)
	c = mix(c, p.near, clamp01(0.5-(v-near)/pv))
	eu, ev := math.Abs(u-0.5)*2, math.Abs(v-0.5)*2
	return scale(c, 0.9*(1-0.35*(eu*eu*eu+ev*ev*ev)))
}

// selectedBackground is pic0: the logo on the right in a pool of light,
// a strip of film sweeping up under it, the left side left dark for the
// console's own title, Play button and cards.
func selectedBackground(w, h int) *canvas {
	c := &canvas{w: w, h: h, px: make([]rgb, w*h)}
	logo := placed{cx: 2640, cy: 930, w: 1460, shapes: wordmark()}
	halo := blurred(16, 5, logo.solid)
	strip := filmThrough(pt{1150, 2330}, pt{2350, 1740}, pt{3990, 1420}, 118)
	strip.fadeIn = 1100
	back := filmThrough(pt{1900, -140}, pt{2900, 260}, pt{3990, 330}, 70)
	back.alpha, back.fadeIn, back.firstPic = 0.32, 900, 40
	c.paint(func(x, y, px float64, p *rgb) {
		u, v := x/bgW, y/bgH
		col := mix(rgb{8, 10, 15}, rgb{13, 19, 32}, smooth(0.1, 0.8, u))
		col = scale(col, 1-0.3*smooth(0.55, 1, v))
		col = light(col, glow, 0.30*blob(x, y, 2640, 960, 1300, 760))
		col = light(col, rgb{24, 52, 128}, 0.6*blob(x, y, 3500, 1950, 1000, 520))
		col = back.at(x, y, px, col)
		col = strip.at(x, y, px, col)
		col = light(col, glow, 0.5*halo.at(x, y))
		col = logo.draw(x, y, px, col)
		dx, dy := (x-2500)/bgW, (y-1000)/bgH
		*p = scale(col, 1-0.45*smooth(0.25, 0.75, math.Hypot(dx*1.2, dy)))
	})
	return c
}

// launchBackground is pic1: the logo alone in the middle, fading to the
// colour FLEX itself opens on.
func launchBackground(w, h int) *canvas {
	c := &canvas{w: w, h: h, px: make([]rgb, w*h)}
	logo := placed{cx: 1920, cy: 1060, w: 1500, shapes: wordmark()}
	halo := blurred(16, 5, logo.solid)
	c.paint(func(x, y, px float64, p *rgb) {
		col := light(screenBG, glow, 0.22*blob(x, y, 1920, 1060, 1500, 800))
		col = light(col, glow, 0.45*halo.at(x, y))
		*p = logo.draw(x, y, px, col)
	})
	return c
}
