package rdp

import "anykvm/internal/pixelops"

// ClearCodec caches are per connection, shared across bitmap updates. Entries
// contain BGR pixels, not characters or font identifiers. See MS-RDPEGFX
// 2.2.4.1 and 3.3.1.9–3.3.1.13.
type clearCache struct {
	slots  []string
	lookup map[string]int
	cursor int
}

func newClearCache(size int) clearCache {
	return clearCache{slots: make([]string, size), lookup: make(map[string]int)}
}

// Replacing a slot also removes its old lookup. The slot cursor is part of the
// wire protocol for bars; glyph indices are explicitly transmitted instead.
func (c *clearCache) put(key string) (int, string) {
	index := c.cursor
	old := c.slots[index]
	if old != "" && c.lookup[old] == index {
		delete(c.lookup, old)
	}
	c.slots[index], c.lookup[key] = key, index
	c.cursor = (index + 1) % len(c.slots)
	return index, old
}

type clearEncoder struct {
	sequence byte
	reset    bool
	glyph    clearCache
	bar      clearCache
	short    clearCache
}

func newClearEncoder() *clearEncoder {
	return &clearEncoder{reset: true, glyph: newClearCache(4000), bar: newClearCache(32768), short: newClearCache(16384)}
}

func clearBGR(pixels []byte) []byte {
	return pixelops.PackBGR(make([]byte, len(pixels)/4*3), pixels)
}

func (e *clearEncoder) encode(pixels []byte, w, h int) []byte {
	flags, sequence := byte(0), e.sequence
	e.sequence++
	// Reset before the short cursor exhausts its 14-bit index space. Clearing
	// local lookups is conservative: the peer only resets the two cursors.
	if e.reset || e.short.cursor+w*((h+51)/52) >= len(e.short.slots) {
		e.bar, e.short = newClearCache(32768), newClearCache(16384)
		e.reset = false
		flags |= 4
	}
	var glyphKey string
	if w*h <= 1024 {
		glyphKey = string(join(u16(w), u16(h), clearBGR(pixels)))
		if index, ok := e.glyph.lookup[glyphKey]; ok {
			return join([]byte{flags | 3, sequence}, u16(index))
		}
	}

	payload := encodeClearCodec(pixels, w, h, sequence)[2:]
	if bands := e.encodeBands(pixels, w, h, len(payload)); bands != nil {
		payload = bands
	}
	head := []byte{flags, sequence}
	if glyphKey != "" {
		index, _ := e.glyph.put(glyphKey)
		head[0] |= 1
		head = append(head, u16(index)...)
	}
	return append(head, payload...)
}

func (e *clearEncoder) encodeBands(pixels []byte, w, h, limit int) []byte {
	backgrounds, worthwhile := e.bandEstimate(pixels, w, h, limit)
	if !worthwhile {
		return nil
	}
	// Band generation updates its caches tentatively. If the residual/raw
	// candidate wins, undo every insertion so unsent entries cannot be hit.
	type insertion struct {
		cache          *clearCache
		index          int
		key, old       string
		previous, prev int
		present        bool
	}
	var changes []insertion
	put := func(cache *clearCache, key string) {
		previous, present := cache.lookup[key]
		prev := cache.cursor
		index, old := cache.put(key)
		changes = append(changes, insertion{cache, index, key, old, previous, prev, present})
	}
	var bands []byte
	column := make([]byte, 0, 52*3)
	for y := 0; y < h; y += 52 {
		height := min(52, h-y)
		bg := backgrounds[y/52]
		bands = append(bands, join(u16(0), u16(w-1), u16(y), u16(y+height-1), bg[:])...)
		for x := 0; x < w; x++ {
			column = column[:0]
			for row := y; row < y+height; row++ {
				off := (row*w + x) * 4
				column = append(column, pixels[off:off+3]...)
			}
			key := string(column)
			if index, ok := e.bar.lookup[key]; ok {
				bands = append(bands, u16(0x8000|index)...)
				continue
			}
			on, off := 0, height
			for on < off && [3]byte{column[on*3], column[on*3+1], column[on*3+2]} == bg {
				on++
			}
			for off > on && [3]byte{column[(off-1)*3], column[(off-1)*3+1], column[(off-1)*3+2]} == bg {
				off--
			}
			// Prefix makes an empty (all-background) short bar distinct from
			// an unused cache slot.
			shortKey := "\x00" + string(column[on*3:off*3])
			if index, ok := e.short.lookup[shortKey]; ok {
				bands = append(bands, u16(0x4000|index)...)
				bands = append(bands, byte(on))
			} else {
				bands = append(bands, u16(on|off<<8)...)
				bands = append(bands, shortKey[1:]...)
				put(&e.short, shortKey)
			}
			put(&e.bar, key)
		}
	}
	if 12+len(bands) >= limit {
		for i := len(changes) - 1; i >= 0; i-- {
			c := changes[i]
			delete(c.cache.lookup, c.key)
			if c.present {
				c.cache.lookup[c.key] = c.previous
			}
			c.cache.slots[c.index], c.cache.cursor = c.old, c.prev
			if c.old != "" {
				c.cache.lookup[c.old] = c.index
			}
		}
		return nil
	}
	return join(u32(0), u32(uint32(len(bands))), u32(0), bands)
}

// Sample backgrounds and column costs before building a speculative band
// stream. Cache hits and repeated columns are useful; unique full-height
// columns would expand noisy pixels. This is an encoder heuristic, not a wire
// format restriction: rejected bands simply use the lossless residual/raw path.
func (e *clearEncoder) bandEstimate(pixels []byte, w, h, limit int) ([][3]byte, bool) {
	count := (h + 51) / 52
	if 12+count*(11+2*w) >= limit {
		return nil, false
	}
	backgrounds := make([][3]byte, count)
	estimated := 12 + 11*count
	var column [52 * 3]byte
	for band := range backgrounds {
		y, height := band*52, min(52, h-band*52)
		colors := make(map[[3]byte]int)
		best := 0
		for row := y; row < y+height; row += max(1, height/8) {
			for x := 0; x < w; x += max(1, w/8) {
				i := (row*w + x) * 4
				color := [3]byte{pixels[i], pixels[i+1], pixels[i+2]}
				colors[color]++
				if colors[color] > best {
					backgrounds[band], best = color, colors[color]
				}
			}
		}
		seenFull, seenShort := make(map[string]bool), make(map[string]bool)
		sample, cost := min(w, 16), 0
		for x := 0; x < sample; x++ {
			for row := 0; row < height; row++ {
				i := ((y+row)*w + x) * 4
				copy(column[row*3:], pixels[i:i+3])
			}
			key := string(column[:height*3])
			_, fullHit := e.bar.lookup[key]
			if fullHit || seenFull[key] {
				cost += 2
				continue
			}
			seenFull[key] = true
			on, off := 0, height
			bg := backgrounds[band]
			for on < off && [3]byte{column[on*3], column[on*3+1], column[on*3+2]} == bg {
				on++
			}
			for off > on && [3]byte{column[(off-1)*3], column[(off-1)*3+1], column[(off-1)*3+2]} == bg {
				off--
			}
			short := "\x00" + string(column[on*3:off*3])
			_, shortHit := e.short.lookup[short]
			if shortHit || seenShort[short] {
				cost += 3
			} else {
				cost += 2 + (off-on)*3
				seenShort[short] = true
			}
		}
		estimated += cost * w / sample
	}
	return backgrounds, estimated < limit
}
