package gamestream

import (
	"embed"
	"strconv"
	"strings"
	"sync"
)

// Keep the original upstream BDF data and both font authors' notices together.
//
//go:embed coverfonts/*.bdf coverfonts/*.txt
var coverFontFiles embed.FS

type coverBitmapFont struct {
	width, height int
	glyphs        map[rune][]uint16 // each row's low bit is the leftmost pixel
}

var coverFonts = sync.OnceValue(func() []*coverBitmapFont {
	var fonts []*coverBitmapFont
	for _, name := range []string{"Tamzen5x9b.bdf", "Tamzen6x12b.bdf", "Tamzen8x16b.bdf", "Tamzen10x20b.bdf"} {
		data, err := coverFontFiles.ReadFile("coverfonts/" + name)
		if err != nil {
			panic(err)
		}
		fonts = append(fonts, parseCoverFont(string(data)))
	}
	return fonts
})

// Read the fixed-cell BDF format of the pinned Tamzen fonts. Bitmap rows are
// copied exactly, including blank padding, without rasterizing font outlines.
func parseCoverFont(data string) *coverBitmapFont {
	f := &coverBitmapFont{glyphs: make(map[rune][]uint16)}
	number := func(s string) int {
		n, err := strconv.Atoi(s)
		if err != nil {
			panic(err)
		}
		return n
	}
	ascent, descent, encoding := 0, 0, -1
	w, h, x, y, row := 0, 0, 0, 0, -1
	var glyph []uint16
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "FONT_ASCENT":
			ascent = number(fields[1])
		case "FONT_DESCENT":
			descent = number(fields[1])
		case "ENCODING":
			encoding = number(fields[1])
		case "DWIDTH":
			width := number(fields[1])
			if f.width != 0 && f.width != width {
				panic("cover font must be monospaced")
			}
			f.width, f.height = width, ascent+descent
		case "BBX":
			w, h, x, y = number(fields[1]), number(fields[2]), number(fields[3]), number(fields[4])
		case "BITMAP":
			glyph = make([]uint16, f.height)
			row = 0
		case "ENDCHAR":
			if row != h || encoding < 0 {
				panic("invalid embedded cover font glyph")
			}
			f.glyphs[rune(encoding)] = glyph
			row = -1
		default:
			if row < 0 {
				continue
			}
			bits, err := strconv.ParseUint(fields[0], 16, 64)
			if err != nil {
				panic(err)
			}
			gy := ascent - y - h + row
			for gx := 0; gx < w; gx++ {
				if bits&(1<<uint(len(fields[0])*4-1-gx)) == 0 {
					continue
				}
				if x+gx < 0 || x+gx >= f.width || gy < 0 || gy >= f.height {
					panic("embedded cover font clips a glyph")
				}
				glyph[gy] |= 1 << uint(x+gx)
			}
			row++
		}
	}
	return f
}

func coverFontForCell(width, height int) (*coverBitmapFont, int) {
	var best *coverBitmapFont
	bestScale, bestArea := 0, 0
	for _, f := range coverFonts() {
		scale := min(width/f.width, height/f.height)
		area := f.width * f.height * scale * scale
		if area > bestArea {
			best, bestScale, bestArea = f, scale, area
		}
	}
	return best, bestScale
}
