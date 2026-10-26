package tray

import "math"

// Dessin des icônes, sans fichier .ico : un nuage et une pastille d'état, calculés
// pixel par pixel (sur-échantillonnage 4×4) à la taille demandée par Windows.
// Portable et testable ; la conversion en HICON est dans icons_windows.go.

type rgba struct{ R, G, B, A float64 }

var (
	cloudColor = rgba{0x4A / 255.0, 0x5A / 255.0, 0x70 / 255.0, 1}
	cloudGrey  = rgba{0x9A / 255.0, 0xA3 / 255.0, 0xAD / 255.0, 1}
	white      = rgba{1, 1, 1, 1}
	green      = rgba{0x1E / 255.0, 0x9E / 255.0, 0x48 / 255.0, 1}
	blue       = rgba{0x1F / 255.0, 0x6F / 255.0, 0xDE / 255.0, 1}
	orange     = rgba{0xE0 / 255.0, 0x7A / 255.0, 0x00 / 255.0, 1}
	red        = rgba{0xD3 / 255.0, 0x2F / 255.0, 0x2F / 255.0, 1}
)

type shape func(x, y float64) bool

type layer struct {
	in shape
	c  rgba
}

func circle(cx, cy, r float64) shape {
	return func(x, y float64) bool { return (x-cx)*(x-cx)+(y-cy)*(y-cy) <= r*r }
}

func union(s ...shape) shape {
	return func(x, y float64) bool {
		for _, f := range s {
			if f(x, y) {
				return true
			}
		}
		return false
	}
}

// stroke : segment de (x1,y1) à (x2,y2), d'épaisseur w.
func stroke(x1, y1, x2, y2, w float64) shape {
	return func(x, y float64) bool {
		dx, dy := x2-x1, y2-y1
		t := ((x-x1)*dx + (y-y1)*dy) / (dx*dx + dy*dy)
		t = math.Max(0, math.Min(1, t))
		px, py := x1+t*dx-x, y1+t*dy-y
		return px*px+py*py <= w*w/4
	}
}

func ring(cx, cy, r, w float64) shape {
	return func(x, y float64) bool {
		d := math.Hypot(x-cx, y-cy)
		return d <= r && d >= r-w
	}
}

func cloud() shape {
	return union(
		circle(0.30, 0.56, 0.20),
		circle(0.52, 0.42, 0.27),
		circle(0.75, 0.57, 0.19),
		func(x, y float64) bool { return x >= 0.30 && x <= 0.75 && y >= 0.50 && y <= 0.76 },
	)
}

// layers renvoie les couches de l'icône d'un état ; frame anime l'état « en cours ».
func layers(icon Icon, frame int) []layer {
	const bx, by, br = 0.72, 0.72, 0.27
	ls := []layer{{cloud(), cloudColor}}
	badge := func(c rgba, glyph ...shape) []layer {
		out := []layer{{circle(bx, by, br+0.05), white}, {circle(bx, by, br), c}}
		for _, g := range glyph {
			out = append(out, layer{g, white})
		}
		return out
	}
	const w = 0.085
	switch icon {
	case IconOK:
		ls = append(ls, badge(green, stroke(0.60, 0.73, 0.69, 0.82, w), stroke(0.69, 0.82, 0.85, 0.62, w))...)
	case IconRunning:
		off := 0.0
		if frame%2 == 1 {
			off = -0.04
		}
		ls = append(ls, badge(blue,
			stroke(bx, 0.85+off, bx, 0.60+off, w),
			stroke(bx, 0.59+off, 0.63, 0.69+off, w),
			stroke(bx, 0.59+off, 0.81, 0.69+off, w))...)
	case IconWarning:
		ls = append(ls, badge(orange, stroke(bx, 0.59, bx, 0.74, w), circle(bx, 0.83, w/1.6))...)
	case IconError:
		ls = append(ls, badge(red, stroke(0.63, 0.63, 0.81, 0.81, w), stroke(0.81, 0.63, 0.63, 0.81, w))...)
	case IconLate:
		ls = append(ls, badge(orange, ring(bx, by, 0.17, 0.06), stroke(bx, by, bx, 0.61, 0.06), stroke(bx, by, 0.80, by, 0.06))...)
	case IconSuspended, IconUnknown:
		ls[0].c = cloudGrey
		if icon == IconSuspended {
			ls = append(ls, badge(cloudColor, stroke(0.66, 0.63, 0.66, 0.81, w), stroke(0.78, 0.63, 0.78, 0.81, w))...)
		}
	}
	return ls
}

// Pixels renvoie l'icône en BGRA (alpha non prémultiplié), lignes de haut en bas.
func Pixels(icon Icon, frame, size int) []byte {
	ls := layers(icon, frame)
	out := make([]byte, size*size*4)
	const ss = 4
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var r, g, b, a float64 // prémultipliés
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := (float64(px) + (float64(sx)+0.5)/ss) / float64(size)
					y := (float64(py) + (float64(sy)+0.5)/ss) / float64(size)
					for i := len(ls) - 1; i >= 0; i-- {
						if ls[i].in(x, y) {
							c := ls[i].c
							r, g, b, a = r+c.R*c.A, g+c.G*c.A, b+c.B*c.A, a+c.A
							break
						}
					}
				}
			}
			n := float64(ss * ss)
			r, g, b, a = r/n, g/n, b/n, a/n
			o := (py*size + px) * 4
			if a > 0 {
				out[o], out[o+1], out[o+2] = byte(b/a*255+0.5), byte(g/a*255+0.5), byte(r/a*255+0.5)
			}
			out[o+3] = byte(a*255 + 0.5)
		}
	}
	return out
}
