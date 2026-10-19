package gamestream

import (
	"bytes"
	"embed"
	"encoding/json"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

// Include both the source artwork and its notices in the standalone executable.
//
//go:embed coverlogos/*.txt coverlogos/*.json
var coverLogoFiles embed.FS

const (
	coverWidth     = 628
	coverHeight    = 888
	logoWidth      = 564
	logoHeight     = 332
	logoTop        = 16
	webCoverWidth  = 920
	webCoverHeight = 430
)

var coverBackground = color.RGBA{20, 28, 43, 255}

// Tango terminal colors, matching the approved Fastfetch previews. Bold selects
// the bright half of the palette; a reset restores normal white on no background.
var coverANSI = [...]color.RGBA{
	{46, 52, 54, 255}, {204, 0, 0, 255}, {78, 154, 6, 255}, {196, 160, 0, 255},
	{52, 101, 164, 255}, {117, 80, 123, 255}, {6, 152, 154, 255}, {211, 215, 207, 255},
	{85, 87, 83, 255}, {239, 41, 41, 255}, {138, 226, 52, 255}, {252, 233, 79, 255},
	{114, 159, 207, 255}, {173, 127, 168, 255}, {52, 226, 226, 255}, {238, 238, 236, 255},
}

// Indexed colors carry a 256 offset so bold does not brighten explicit 256-color
// values. True colors carry a high bit, independent of the terminal palette.
func coverColor(ink int) color.RGBA {
	if ink >= 1<<24 {
		return color.RGBA{uint8(ink >> 16), uint8(ink >> 8), uint8(ink), 255}
	}
	if ink >= 256 {
		ink -= 256
	}
	if ink < 16 {
		return coverANSI[ink]
	}
	if ink < 232 {
		levels := [...]uint8{0, 95, 135, 175, 215, 255}
		n := ink - 16
		return color.RGBA{levels[n/36], levels[(n/6)%6], levels[n%6], 255}
	}
	gray := uint8(8 + (ink-232)*10)
	return color.RGBA{gray, gray, gray, 255}
}

type coverLogo struct {
	File   string   `json:"file"`
	Colors []string `json:"colors"`
}

var coverLogos = sync.OnceValue(func() map[string]coverLogo {
	data, err := coverLogoFiles.ReadFile("coverlogos/logos.json")
	if err != nil {
		panic(err)
	}
	var logos map[string]coverLogo
	if err := json.Unmarshal(data, &logos); err != nil {
		panic(err)
	}
	return logos
})

type coverCell struct {
	char   rune
	fg, bg int // bg == -1 means the cover background
	hidden bool
}

// parseCoverLogo interprets Fastfetch's $1..$9 SGR slots and $$ escapes. These
// are embedded text assets, not terminal output: no terminal emulator is needed.
func parseCoverLogo(text string, colors []string) [][]coverCell {
	rows := [][]coverCell{{}}
	fg, bg, hidden, bright := 7, -1, false, true
	apply := func(style string) {
		var codes []int
		for _, part := range strings.Split(style, ";") {
			code, err := strconv.Atoi(part)
			if err != nil {
				panic("invalid embedded cover color: " + style)
			}
			codes = append(codes, code)
		}
		for i := 0; i < len(codes); i++ {
			code := codes[i]
			switch {
			case code == 0:
				fg, bg, hidden, bright = 7, -1, false, false
			case code == 1:
				bright = true
			case code == 22:
				bright = false
			case code == 8:
				hidden = true
			case code == 28:
				hidden = false
			case code >= 30 && code <= 37:
				fg = code - 30
			case code >= 90 && code <= 97:
				fg = code - 90 + 8
			case code == 39:
				fg = 7
			case code >= 40 && code <= 47:
				bg = code - 40
			case code >= 100 && code <= 107:
				bg = code - 100 + 8
			case code == 49:
				bg = -1
			case code == 38 || code == 48:
				var ink int
				if i+2 < len(codes) && codes[i+1] == 5 && codes[i+2] >= 0 && codes[i+2] <= 255 {
					ink = 256 + codes[i+2]
					i += 2
				} else if i+4 < len(codes) && codes[i+1] == 2 {
					ink = 1 << 24
					for _, component := range codes[i+2 : i+5] {
						if component < 0 || component > 255 {
							panic("invalid embedded cover color: " + style)
						}
					}
					ink |= codes[i+2]<<16 | codes[i+3]<<8 | codes[i+4]
					i += 4
				} else {
					panic("invalid embedded cover color: " + style)
				}
				if code == 38 {
					fg = ink
				} else {
					bg = ink
				}
			}
		}
	}
	if len(colors) > 0 {
		apply(colors[0])
	}
	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		if c == '$' && i+1 < len(runes) {
			next := runes[i+1]
			if next >= '1' && next <= '9' {
				i++
				if index := int(next - '1'); index < len(colors) {
					apply(colors[index])
				}
				continue
			}
			if next == '$' {
				i++
			}
		}
		if c == '\n' {
			rows = append(rows, nil)
		} else if c != '\r' {
			ink := fg
			if bright && ink < 8 {
				ink += 8
			}
			rows[len(rows)-1] = append(rows[len(rows)-1], coverCell{c, ink, bg, hidden})
		}
	}
	// Crop terminal padding using visible ink, including colored spaces.
	left, right, top, bottom := len(runes), 0, len(rows), 0
	for y, row := range rows {
		for x, cell := range row {
			if (!unicode.IsSpace(cell.char) && cell.char != '⠀' && !cell.hidden) || cell.bg >= 0 {
				left, right = min(left, x), max(right, x+1)
				top, bottom = min(top, y), max(bottom, y+1)
			}
		}
	}
	if right == 0 {
		return nil
	}
	trimmed := make([][]coverCell, bottom-top)
	for y, row := range rows[top:bottom] {
		trimmed[y] = make([]coverCell, right-left)
		for x := range trimmed[y] {
			trimmed[y][x] = coverCell{' ', 7, -1, false}
			if left+x < len(row) {
				trimmed[y][x] = row[left+x]
			}
		}
	}
	return trimmed
}

// Bits run top-left, top-right, bottom-left, bottom-right. Block characters are
// rectangles on this lattice, never font glyphs, so adjacent blocks have no gaps.
func coverBlockMask(c rune) uint8 {
	switch c {
	case '█':
		return 15
	case '▀':
		return 3
	case '▄':
		return 12
	case '▌':
		return 5
	case '▐':
		return 10
	case '▖':
		return 4
	case '▗':
		return 8
	case '▘':
		return 1
	case '▝':
		return 2
	case '▙':
		return 13
	case '▚':
		return 9
	case '▛':
		return 7
	case '▜':
		return 11
	case '▞':
		return 6
	case '▟':
		return 14
	}
	return 0
}

func drawCoverBlock(img *image.RGBA, rect image.Rectangle, mask uint8, ink color.Color) {
	w, h := rect.Dx()/2, rect.Dy()/2
	for q := 0; q < 4; q++ {
		if mask&(1<<q) != 0 {
			r := image.Rect(0, 0, w, h).Add(rect.Min).Add(image.Pt((q%2)*w, (q/2)*h))
			draw.Draw(img, r, image.NewUniform(ink), image.Point{}, draw.Src)
		}
	}
}

// Draw the Unicode character's eight source dots
// directly instead of substituting a missing-glyph box or changing the artwork.
func drawCoverBraille(img *image.RGBA, rect image.Rectangle, c rune, ink color.Color) {
	unit := rect.Dx() / 2
	size := max(1, unit/2)
	positions := [...]image.Point{{0, 0}, {0, 1}, {0, 2}, {1, 0}, {1, 1}, {1, 2}, {0, 3}, {1, 3}}
	for bit, p := range positions {
		if (c-'⠀')&(1<<bit) != 0 {
			origin := rect.Min.Add(image.Pt(p.X*unit+(unit-size)/2, p.Y*unit+(unit-size)/2))
			draw.Draw(img, image.Rect(0, 0, size, size).Add(origin), image.NewUniform(ink), image.Point{}, draw.Src)
		}
	}
}

func renderMachineBoxArt(platform string) []byte {
	img := image.NewRGBA(image.Rect(0, 0, coverWidth, coverHeight))
	draw.Draw(img, img.Bounds(), image.NewUniform(coverBackground), image.Point{}, draw.Src)
	if logo, ok := coverLogos()[platform]; ok {
		text, err := coverLogoFiles.ReadFile("coverlogos/" + logo.File)
		if err != nil {
			panic(err)
		}
		drawCoverLogo(img, parseCoverLogo(string(text), logo.Colors))
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		panic(err)
	}
	return out.Bytes()
}

func renderWebTargetCover(platform string) []byte {
	img := image.NewRGBA(image.Rect(0, 0, webCoverWidth, webCoverHeight))
	draw.Draw(img, img.Bounds(), image.NewUniform(coverBackground), image.Point{}, draw.Src)
	if logo, ok := coverLogos()[platform]; ok {
		text, err := coverLogoFiles.ReadFile("coverlogos/" + logo.File)
		if err != nil {
			panic(err)
		}
		// Leave a balanced margin around the logo instead of reserving title
		// space below it, as the browser card renders the title separately.
		drawCoverLogoInRect(img, parseCoverLogo(string(text), logo.Colors), image.Rect(36, 36, webCoverWidth-36, webCoverHeight-36))
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		panic(err)
	}
	return out.Bytes()
}

func coverCellSize(rows [][]coverCell) (int, int) {
	return coverCellSizeFor(rows, logoWidth, logoHeight)
}

func coverCellSizeFor(rows [][]coverCell, width, height int) (int, int) {
	unit := min(width/(2*len(rows[0])), height/(4*len(rows)))
	if unit < 1 {
		panic("embedded cover logo exceeds the drawing area")
	}
	cw, ch := unit*2, unit*4
	// Dense ASCII artwork can fit a 5x10 cell even when a 6x12 cell cannot.
	// Keep block/Braille grids even, since their subcells must remain exact.
	if cw < 5 {
		ordinary, geometric := false, false
		for _, row := range rows {
			for _, cell := range row {
				if cell.hidden || unicode.IsSpace(cell.char) {
					continue
				}
				if coverBlockMask(cell.char) != 0 || (cell.char >= '⠀' && cell.char <= '⣿') {
					geometric = true
				} else {
					ordinary = true
				}
			}
		}
		if ordinary && !geometric && len(rows[0])*5 <= width && len(rows)*10 <= height {
			cw, ch = 5, 10
		}
	}
	return cw, ch
}

func drawCoverLogo(img *image.RGBA, rows [][]coverCell) {
	drawCoverLogoInRect(img, rows, image.Rect(0, logoTop, coverWidth, logoTop+logoHeight))
}

func drawCoverLogoInRect(img *image.RGBA, rows [][]coverCell, bounds image.Rectangle) {
	if len(rows) == 0 {
		return
	}
	// A terminal cell has a 1:2 aspect ratio. Each quarter is unit x 2*unit
	// physical pixels; integer sizing keeps every Unicode block edge exact.
	cw, ch := coverCellSizeFor(rows, bounds.Dx(), bounds.Dy())
	ox := bounds.Min.X + (bounds.Dx()-len(rows[0])*cw)/2
	oy := bounds.Min.Y + (bounds.Dy()-len(rows)*ch)/2
	bitmap, scale := coverFontForCell(cw, ch)
	glyphs := make(map[rune]*image.Alpha)
	for y, row := range rows {
		for x, cell := range row {
			r := image.Rect(0, 0, cw, ch).Add(image.Pt(ox+x*cw, oy+y*ch))
			if cell.bg >= 0 {
				draw.Draw(img, r, image.NewUniform(coverColor(cell.bg)), image.Point{}, draw.Src)
			}
			if cell.hidden || unicode.IsSpace(cell.char) || cell.char == '⠀' {
				continue
			}
			ink := image.NewUniform(coverColor(cell.fg))
			if cell.char >= '⠀' && cell.char <= '⣿' {
				drawCoverBraille(img, r, cell.char, ink.C)
				continue
			}
			if mask := coverBlockMask(cell.char); mask != 0 {
				drawCoverBlock(img, r, mask, ink.C)
				continue
			}
			mask := glyphs[cell.char]
			if mask == nil {
				if bitmap == nil {
					panic("embedded cover logo has no space for a Tamzen glyph")
				}
				mask = image.NewAlpha(image.Rect(0, 0, cw, ch))
				glyph, ok := bitmap.glyphs[cell.char]
				if !ok {
					panic("unsupported embedded cover glyph: " + string(cell.char))
				}
				x0, y0 := (cw-bitmap.width*scale)/2, (ch-bitmap.height*scale)/2
				for gy, bits := range glyph {
					for gx := 0; gx < bitmap.width; gx++ {
						if bits&(1<<gx) != 0 {
							rect := image.Rect(x0+gx*scale, y0+gy*scale, x0+(gx+1)*scale, y0+(gy+1)*scale)
							draw.Draw(mask, rect, image.White, image.Point{}, draw.Src)
						}
					}
				}
				glyphs[cell.char] = mask
			}
			draw.DrawMask(img, r, ink, image.Point{}, mask, image.Point{}, draw.Over)
		}
	}
}
