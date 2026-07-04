//go:build windows

package walkui

// Colored icons, drawn programmatically into RGBA bitmaps (no asset files, no
// font emoji). Categories are tinted folders; the nav roots and toolbar get
// simple geometric glyphs in the same palette the old UI used.

import (
	"image"
	"image/color"
	"math"
	"sync"

	"github.com/lxn/walk"
)

func nrgb(v uint32) color.NRGBA {
	return color.NRGBA{R: byte(v >> 16), G: byte(v >> 8), B: byte(v), A: 0xff}
}

var (
	catIconColors = map[string]color.NRGBA{
		"General":    nrgb(0xe6c264), // manila
		"Compressed": nrgb(0xcf9a5a), // tan
		"Documents":  nrgb(0x6aa6e6), // blue
		"Music":      nrgb(0xd07ad0), // magenta
		"Video":      nrgb(0xe07a7a), // red
		"Programs":   nrgb(0x86c2c0), // teal
		"Images":     nrgb(0x6fc06a), // green
	}
	navIconColors = map[string]color.NRGBA{
		"all":        nrgb(0x6aa6e6),
		"unfinished": nrgb(0xe0b84a),
		"finished":   nrgb(0x57c06a),
	}
	toolIconColors = map[string]color.NRGBA{
		"add":          nrgb(0x57c06a),
		"resume":       nrgb(0x57c06a),
		"stop":         nrgb(0xe0b84a),
		"stopall":      nrgb(0xe07a7a),
		"delete":       nrgb(0xe07a7a),
		"delcompleted": nrgb(0xe0a55a),
	}
)

// bitmap cache (built lazily on the GUI thread, reused for every cell/paint).
var (
	bmpMu    sync.Mutex
	bmpCache = map[string]*walk.Bitmap{}
)

func cachedBitmap(key string, draw func(c *canvas)) *walk.Bitmap {
	bmpMu.Lock()
	defer bmpMu.Unlock()
	if b, ok := bmpCache[key]; ok {
		return b
	}
	c := newCanvas(16)
	draw(c)
	b, err := walk.NewBitmapFromImageForDPI(c.img, 96)
	if err != nil {
		return nil
	}
	bmpCache[key] = b
	return b
}

func toolBitmap(key string) *walk.Bitmap {
	col := toolIconColors[key]
	return cachedBitmapN("tool:"+key, 20, func(c *canvas) { drawTool(c, key, col) })
}

func catBitmap(name string) *walk.Bitmap {
	col, ok := catIconColors[name]
	if !ok {
		col = nrgb(0x9aa0a4)
	}
	return cachedBitmap("cat:"+name, func(c *canvas) { drawFolder(c, col) })
}

func navBitmap(key string) *walk.Bitmap {
	col := navIconColors[key]
	return cachedBitmap("nav:"+key, func(c *canvas) { drawNav(c, key, col) })
}

func cachedBitmapN(key string, n int, draw func(c *canvas)) *walk.Bitmap {
	bmpMu.Lock()
	defer bmpMu.Unlock()
	if b, ok := bmpCache[key]; ok {
		return b
	}
	c := newCanvas(n)
	draw(c)
	b, err := walk.NewBitmapFromImageForDPI(c.img, 96)
	if err != nil {
		return nil
	}
	bmpCache[key] = b
	return b
}

// ---- glyph drawing -------------------------------------------------------

type canvas struct {
	img  *image.RGBA
	n    int
	unit float64 // n/16, so coordinates can be authored on a 16-grid
}

func newCanvas(n int) *canvas {
	return &canvas{img: image.NewRGBA(image.Rect(0, 0, n, n)), n: n, unit: float64(n) / 16}
}

func (c *canvas) s(v float64) int { return int(math.Round(v * c.unit)) }

func (c *canvas) set(x, y int, col color.NRGBA) {
	if x < 0 || y < 0 || x >= c.n || y >= c.n {
		return
	}
	c.img.Set(x, y, col)
}

// rect fills a rectangle given on the 16-grid (inclusive-exclusive).
func (c *canvas) rect(x0, y0, x1, y1 float64, col color.NRGBA) {
	ax, ay, bx, by := c.s(x0), c.s(y0), c.s(x1), c.s(y1)
	for y := ay; y < by; y++ {
		for x := ax; x < bx; x++ {
			c.set(x, y, col)
		}
	}
}

// disc fills a circle centered on the 16-grid.
func (c *canvas) disc(cx, cy, r float64, col color.NRGBA) {
	pcx, pcy, pr := cx*c.unit, cy*c.unit, r*c.unit
	for y := 0; y < c.n; y++ {
		for x := 0; x < c.n; x++ {
			dx, dy := float64(x)+0.5-pcx, float64(y)+0.5-pcy
			if dx*dx+dy*dy <= pr*pr {
				c.set(x, y, col)
			}
		}
	}
}

// line stamps a thick line between two 16-grid points.
func (c *canvas) line(x0, y0, x1, y1, thick float64, col color.NRGBA) {
	ax, ay, bx, by := x0*c.unit, y0*c.unit, x1*c.unit, y1*c.unit
	steps := int(math.Hypot(bx-ax, by-ay)) + 1
	half := thick * c.unit / 2
	for i := 0; i <= steps; i++ {
		t := float64(i) / float64(steps)
		px, py := ax+(bx-ax)*t, ay+(by-ay)*t
		for yy := int(py - half); yy <= int(py+half); yy++ {
			for xx := int(px - half); xx <= int(px+half); xx++ {
				c.set(xx, yy, col)
			}
		}
	}
}

// triRight fills a right-pointing triangle inside the box (x0,y0)-(x1,y1).
func (c *canvas) triRight(x0, y0, x1, y1 float64, col color.NRGBA) {
	ax, ay, bx, by := c.s(x0), c.s(y0), c.s(x1), c.s(y1)
	h := by - ay
	for y := ay; y < by; y++ {
		// width shrinks toward the tip (right edge)
		frac := 1.0 - math.Abs(float64(y-ay)-float64(h)/2)/(float64(h)/2)
		xend := ax + int(float64(bx-ax)*frac)
		for x := ax; x < xend; x++ {
			c.set(x, y, col)
		}
	}
}

func drawFolder(c *canvas, col color.NRGBA) {
	tab := col
	body := col
	// folder tab + body
	c.rect(1.5, 4, 7, 6.5, tab)
	c.rect(1.5, 5.5, 14.5, 13.5, body)
	// thin highlight along the top of the body for depth
	hl := color.NRGBA{R: minB(col.R, 40), G: minB(col.G, 40), B: minB(col.B, 40), A: 0xff}
	c.rect(1.5, 5.5, 14.5, 6.3, hl)
}

func minB(v byte, add int) byte {
	n := int(v) + add
	if n > 255 {
		n = 255
	}
	return byte(n)
}

func drawNav(c *canvas, key string, col color.NRGBA) {
	switch key {
	case "all": // download arrow
		c.rect(7, 2.5, 9, 9, col)
		c.triDown(4, 8, 12, 13.5, col)
		c.rect(3.5, 13.5, 12.5, 15, col)
	case "unfinished": // hourglass
		c.triDown(3.5, 3, 12.5, 8, col)
		c.triUp(3.5, 8, 12.5, 13, col)
		c.rect(3.5, 2.5, 12.5, 3.3, col)
		c.rect(3.5, 12.7, 12.5, 13.5, col)
	case "finished": // check mark
		c.line(3.5, 8.5, 7, 12, 2.4, col)
		c.line(7, 12, 13, 4.5, 2.4, col)
	}
}

// triDown fills a downward triangle inside the box.
func (c *canvas) triDown(x0, y0, x1, y1 float64, col color.NRGBA) {
	ax, ay, bx, by := c.s(x0), c.s(y0), c.s(x1), c.s(y1)
	w := bx - ax
	for y := ay; y < by; y++ {
		frac := 1.0 - float64(y-ay)/float64(by-ay)
		half := int(float64(w) / 2 * frac)
		mid := (ax + bx) / 2
		for x := mid - half; x <= mid+half; x++ {
			c.set(x, y, col)
		}
	}
}

// triUp fills an upward triangle inside the box.
func (c *canvas) triUp(x0, y0, x1, y1 float64, col color.NRGBA) {
	ax, ay, bx, by := c.s(x0), c.s(y0), c.s(x1), c.s(y1)
	w := bx - ax
	for y := ay; y < by; y++ {
		frac := float64(y-ay) / float64(by-ay)
		half := int(float64(w) / 2 * frac)
		mid := (ax + bx) / 2
		for x := mid - half; x <= mid+half; x++ {
			c.set(x, y, col)
		}
	}
}

func drawTool(c *canvas, key string, col color.NRGBA) {
	white := color.NRGBA{0xff, 0xff, 0xff, 0xff}
	switch key {
	case "add":
		c.disc(10, 10, 8, col)
		c.rect(9, 5.5, 11, 14.5, white)
		c.rect(5.5, 9, 14.5, 11, white)
	case "resume":
		c.triRight(6, 4, 15.5, 16, col)
	case "stop": // pause bars
		c.rect(6, 4, 9, 16, col)
		c.rect(11, 4, 14, 16, col)
	case "stopall": // solid square
		c.rect(5, 5, 15, 15, col)
	case "delete":
		drawTrash(c, col, white)
	case "delcompleted":
		drawTrash(c, col, white)
		// little green check overlay (bottom-right)
		g := nrgb(0x57c06a)
		c.line(11, 14, 13, 16.5, 1.8, g)
		c.line(13, 16.5, 17, 11.5, 1.8, g)
	}
}

func drawTrash(c *canvas, col, white color.NRGBA) {
	// handle + lid
	c.rect(8, 3, 12, 5, col)
	c.rect(4.5, 5, 15.5, 7, col)
	// body
	c.rect(6, 7, 14, 17, col)
	// white ribs
	c.rect(7.6, 8.5, 8.4, 15.5, white)
	c.rect(9.6, 8.5, 10.4, 15.5, white)
	c.rect(11.6, 8.5, 12.4, 15.5, white)
}
