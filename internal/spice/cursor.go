package spice

import (
	"fmt"

	"anykvm/internal/capture"
)

const maxCursorDimension = 256

type cursorSnapshot struct {
	// Normalized RGBA from spice-client-glib, not protocol ARGB words.
	rgba                      []byte
	width, height, hotX, hotY int

	known, visible, systemDefault, premultiplied bool

	serial uint64
}

type cursorSession interface {
	cursor() (*cursorSnapshot, error)
}

func (*Device) UsesLocalCursor() bool { return false }

func (d *Device) CaptureCursor() (*capture.CursorState, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	snapshot := &cursorSnapshot{}
	if s, ok := d.session.(cursorSession); ok {
		var err error
		snapshot, err = s.cursor()
		if err != nil {
			return nil, err
		}
		if snapshot == nil {
			snapshot = &cursorSnapshot{}
		}
	}
	state, err := cursorState(*snapshot)
	if err != nil {
		return nil, err
	}
	state.SourceWidth, state.SourceHeight = d.width, d.height
	state.Changed = d.cursorGeneration != d.generation || d.cursorSerial != snapshot.serial
	d.cursorGeneration, d.cursorSerial = d.generation, snapshot.serial
	return state, nil
}

func cursorState(s cursorSnapshot) (*capture.CursorState, error) {
	if !s.known || !s.visible {
		return &capture.CursorState{}, nil
	}
	if s.systemDefault {
		return &capture.CursorState{SystemDefault: true}, nil
	}
	if s.width < 1 || s.height < 1 || s.width > maxCursorDimension || s.height > maxCursorDimension || len(s.rgba) != s.width*s.height*4 {
		return nil, fmt.Errorf("invalid SPICE cursor shape %dx%d", s.width, s.height)
	}
	// Cursor images are small independent protocol resources. Copy/swizzle only
	// their pixels; never materialize or modify the GPU desktop image.
	pixels := make([]byte, len(s.rgba))
	for i := 0; i < len(pixels); i += 4 {
		pixels[i], pixels[i+1], pixels[i+2], pixels[i+3] = s.rgba[i+2], s.rgba[i+1], s.rgba[i], s.rgba[i+3]
	}
	return &capture.CursorState{
		Image: pixels, Width: s.width, Height: s.height,
		HotX:          max(0, min(s.width-1, s.hotX)),
		HotY:          max(0, min(s.height-1, s.hotY)),
		Premultiplied: s.premultiplied,
	}, nil
}
