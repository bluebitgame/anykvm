package vnc

import (
	"bytes"
	"image"

	"anykvm/internal/jpegcodec"
	"anykvm/internal/pixelops"
)

// As in Progressive's PrepareSurface/CommitSurface flow, this baseline is owned
// by one viewer and only committed after the entire framebuffer update writes.
type compressedJPEGState struct {
	source        []byte
	width, height int
	restart       *jpegcodec.RestartJPEG
	tiles         []jpegcodec.JPEGTile
}

func (c *client) prepareCompressedJPEG(frame *frameData, full bool) (*compressedJPEGState, []rectangle, map[rectangle][]byte) {
	return c.prepareCompressedJPEGWithCopies(frame, full, nil)
}

func (c *client) prepareCompressedJPEGWithCopies(frame *frameData, full bool, copies []copyRectangle) (*compressedJPEGState, []rectangle, map[rectangle][]byte) {
	w, h := int(frame.width), int(frame.height)
	old := c.jpegPrevious
	if !full && old != nil && old.width == w && old.height == h &&
		((frame.jpegRegions != nil && frame.jpegRegions == old.restart) ||
			(frame.jpegRegions == nil && bytes.Equal(old.source, frame.data))) {
		return old, nil, nil
	}
	// Do not retain a capture-owned byte slice after sendFrame releases its lease.
	next := &compressedJPEGState{source: bytes.Clone(frame.data), width: w, height: h}
	fallback := func() (*compressedJPEGState, []rectangle, map[rectangle][]byte) {
		return next, []rectangle{{0, 0, w, h}}, nil
	}
	source := frame.jpegRegions
	if source == nil {
		source, _ = jpegcodec.ParseRestartJPEG(next.source)
	}
	if source != nil && source.Width == w && source.Height == h {
		next.restart = source
		cols, rows := (w+63)/64, (h+63)/64
		dirty := make([]bool, cols*rows)
		refresh := full || old == nil || !source.SameParameters(old.restart)
		for y := 0; y < rows; y++ {
			for x := 0; x < cols; x++ {
				r := image.Rect(x*64, y*64, min(w, (x+1)*64), min(h, (y+1)*64))
				dirty[y*cols+x] = refresh || !source.EqualRect(old.restart, r)
			}
		}
		rects := splitJPEGRects(omitCopyDestinations(mergeDirtyGrid(dirty, w, h), copies))
		if source.HasRecordedColors() && c.chooseEncoding(c.pixelFormatSnapshot(), frame) == encTight {
			// Classify before cropping: pure MCU records must go directly to
			// Fill, without synthesizing a JPEG that would immediately be discarded.
			return next, rects, nil
		}
		payloads := make(map[rectangle][]byte, len(rects))
		for _, r := range rects {
			data, err := source.Crop(image.Rect(r.x, r.y, r.x+r.w, r.y+r.h))
			if err != nil {
				return fallback()
			}
			payloads[r] = data
		}
		return next, rects, payloads
	}
	// External MJPEG: one coefficient decode shared by all lossless tile crops.
	// Comparing complete canonical tile JPEGs is exact and includes their tables.
	tiles, tw, th, err := jpegcodec.LosslessJPEGTiles(next.source, pixelops.BlockSize)
	if err != nil || tw != w || th != h {
		if !c.jpegCropLogged {
			c.logf("lossless JPEG tiles unavailable; retaining whole-frame passthrough: %v", err)
			c.jpegCropLogged = true
		}
		return fallback()
	}
	next.tiles = tiles
	refresh := full || old == nil || old.width != w || old.height != h || len(old.tiles) != len(tiles)
	var rects []rectangle
	payloads := make(map[rectangle][]byte)
	for i, tile := range tiles {
		if !refresh && old.tiles[i].Rect == tile.Rect && bytes.Equal(old.tiles[i].Data, tile.Data) {
			continue
		}
		r := rectangle{tile.Rect.Min.X, tile.Rect.Min.Y, tile.Rect.Dx(), tile.Rect.Dy()}
		rects = append(rects, r)
		payloads[r] = tile.Data
	}
	return next, rects, payloads
}
