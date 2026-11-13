package main

// bc7DDS writes a picture in the form the PS5 home screen takes its
// backgrounds in: a DDS file with a DX10 header, BC7_UNORM, one surface, no
// mipmaps. Every 4x4 block uses BC7's mode 6: one pair of colour end points
// (7 bits a channel plus a low bit) with sixteen steps between them, which
// keeps the dark gradients smooth. The pictures are opaque, so both low bits
// are 1 and alpha comes out 255.

import (
	"encoding/binary"
	"image"
	"math"
	"runtime"
	"sync"
)

var bc7Weights = [16]int{0, 4, 9, 13, 17, 21, 26, 30, 34, 38, 43, 47, 51, 55, 60, 64}

type bc7Block [16][3]float64

// endpoint value of a 7-bit code with the low bit 1
func bc7Value(q int) int { return q<<1 | 1 }

func bc7Quant(v [3]float64) (q [3]int) {
	for i, c := range v {
		q[i] = int(math.Max(0, math.Min(127, math.Round((c-1)/2))))
	}
	return q
}

// bc7Assign picks the nearest of the sixteen steps for every texel.
func bc7Assign(px *bc7Block, q0, q1 [3]int) (idx [16]int, total float64) {
	var pal [16][3]float64
	for k, w := range bc7Weights {
		for c := 0; c < 3; c++ {
			pal[k][c] = float64(((64-w)*bc7Value(q0[c]) + w*bc7Value(q1[c]) + 32) >> 6)
		}
	}
	for i, p := range px {
		best, bestErr := 0, math.Inf(1)
		for k := range pal {
			dr, dg, db := p[0]-pal[k][0], p[1]-pal[k][1], p[2]-pal[k][2]
			if e := dr*dr + dg*dg + db*db; e < bestErr {
				best, bestErr = k, e
			}
		}
		idx[i] = best
		total += bestErr
	}
	return idx, total
}

// bc7Ends is the first guess: the extremes of the block along its main axis.
func bc7Ends(px *bc7Block) (e0, e1 [3]float64) {
	var m [3]float64
	for _, p := range px {
		for c := range m {
			m[c] += p[c] / 16
		}
	}
	var cov [3][3]float64
	for _, p := range px {
		for a := 0; a < 3; a++ {
			for b := 0; b < 3; b++ {
				cov[a][b] += (p[a] - m[a]) * (p[b] - m[b])
			}
		}
	}
	axis := [3]float64{1, 1, 1}
	for it := 0; it < 8; it++ {
		var n [3]float64
		for a := 0; a < 3; a++ {
			n[a] = cov[a][0]*axis[0] + cov[a][1]*axis[1] + cov[a][2]*axis[2]
		}
		l := math.Sqrt(n[0]*n[0] + n[1]*n[1] + n[2]*n[2])
		if l < 1e-9 {
			break
		}
		axis = [3]float64{n[0] / l, n[1] / l, n[2] / l}
	}
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, p := range px {
		t := (p[0]-m[0])*axis[0] + (p[1]-m[1])*axis[1] + (p[2]-m[2])*axis[2]
		lo, hi = math.Min(lo, t), math.Max(hi, t)
	}
	for c := range m {
		e0[c], e1[c] = m[c]+lo*axis[c], m[c]+hi*axis[c]
	}
	return e0, e1
}

// bc7Fit finds the end points that best fit the chosen steps (least squares).
func bc7Fit(px *bc7Block, idx [16]int) (e0, e1 [3]float64, ok bool) {
	var aa, ab, bb float64
	var pa, pb [3]float64
	for i, p := range px {
		t := float64(bc7Weights[idx[i]]) / 64
		aa += (1 - t) * (1 - t)
		ab += t * (1 - t)
		bb += t * t
		for c := range pa {
			pa[c] += (1 - t) * p[c]
			pb[c] += t * p[c]
		}
	}
	det := aa*bb - ab*ab
	if math.Abs(det) < 1e-9 {
		return e0, e1, false
	}
	for c := range pa {
		e0[c] = (bb*pa[c] - ab*pb[c]) / det
		e1[c] = (aa*pb[c] - ab*pa[c]) / det
	}
	return e0, e1, true
}

func bc7Encode(px *bc7Block) (out [16]byte) {
	a, b := bc7Ends(px)
	q0, q1 := bc7Quant(a), bc7Quant(b)
	idx, err := bc7Assign(px, q0, q1)
	for it := 0; it < 3 && err > 0; it++ {
		a, b, ok := bc7Fit(px, idx)
		if !ok {
			break
		}
		c0, c1 := bc7Quant(a), bc7Quant(b)
		ci, ce := bc7Assign(px, c0, c1)
		if ce >= err {
			break
		}
		q0, q1, idx, err = c0, c1, ci, ce
	}
	// the first texel's step is stored in 3 bits, so it must be below 8
	if idx[0] >= 8 {
		q0, q1 = q1, q0
		for i := range idx {
			idx[i] = 15 - idx[i]
		}
	}
	n := 0
	put := func(v, count int) {
		for i := 0; i < count; i++ {
			if v>>i&1 != 0 {
				out[n>>3] |= 1 << (n & 7)
			}
			n++
		}
	}
	put(1<<6, 7) // mode 6
	for c := 0; c < 3; c++ {
		put(q0[c], 7)
		put(q1[c], 7)
	}
	put(127, 7) // alpha
	put(127, 7)
	put(1, 1) // low bits
	put(1, 1)
	put(idx[0], 3)
	for _, k := range idx[1:] {
		put(k, 4)
	}
	return out
}

func bc7DDS(img *image.NRGBA) []byte {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	bw, bh := w/4, h/4
	out := make([]byte, 148+bw*bh*16)
	le := binary.LittleEndian
	copy(out, "DDS ")
	le.PutUint32(out[4:], 124)
	le.PutUint32(out[8:], 0x1|0x2|0x4|0x1000|0x20000|0x80000) // caps, height, width, pixel format, mipmap count, linear size
	le.PutUint32(out[12:], uint32(h))
	le.PutUint32(out[16:], uint32(w))
	le.PutUint32(out[20:], uint32(bw*bh*16))
	le.PutUint32(out[28:], 1) // one level: no mipmaps
	le.PutUint32(out[76:], 32)
	le.PutUint32(out[80:], 0x4) // a FourCC follows
	copy(out[84:], "DX10")
	le.PutUint32(out[108:], 0x1000) // a texture
	le.PutUint32(out[128:], 98)     // DXGI_FORMAT_BC7_UNORM
	le.PutUint32(out[132:], 3)      // 2D
	le.PutUint32(out[140:], 1)      // one surface

	rows := make(chan int, bh)
	for y := 0; y < bh; y++ {
		rows <- y
	}
	close(rows)
	var wg sync.WaitGroup
	for i := 0; i < runtime.NumCPU(); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var px bc7Block
			for by := range rows {
				for bx := 0; bx < bw; bx++ {
					for i := 0; i < 16; i++ {
						c := img.NRGBAAt(bx*4+i%4, by*4+i/4)
						px[i] = [3]float64{float64(c.R), float64(c.G), float64(c.B)}
					}
					b := bc7Encode(&px)
					copy(out[148+(by*bw+bx)*16:], b[:])
				}
			}
		}()
	}
	wg.Wait()
	return out
}
