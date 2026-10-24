package rdp

import (
	"anykvm/internal/framebuf"
	"bytes"
	"image"
	"reflect"
	"testing"
	"time"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
	"anykvm/internal/gpuconv"
	"anykvm/internal/jpegcodec"
)

func moveDamage(previous, sequence uint64, moves ...jpegcodec.DesktopMove) *jpegcodec.DesktopDamage {
	return &jpegcodec.DesktopDamage{Stream: 3, Previous: previous, Sequence: sequence, SourceWidth: 192, SourceHeight: 128, Valid: true, Moves: moves}
}

// Replay production SurfaceToSurface PDUs. Scratch and primary pixels are
// separate, exposing mistakes in source selection or overlapping move order.
func replaySurfaceCopies(t *testing.T, wire []byte, pixels []byte, w, h int) []byte {
	t.Helper()
	surfaces := map[int][]byte{1: bytes.Clone(pixels)}
	for len(wire) > 0 {
		if len(wire) < 8 {
			t.Fatal("short graphics header")
		}
		kind, size := le.Uint16(wire), int(le.Uint32(wire[4:]))
		if size < 8 || size > len(wire) {
			t.Fatal("bad graphics length")
		}
		p := wire[8:size]
		wire = wire[size:]
		switch kind {
		case 9:
			surfaces[int(le.Uint16(p))] = make([]byte, w*h*4)
		case 5:
			src, dst := int(le.Uint16(p)), int(le.Uint16(p[2:]))
			x0, y0, x1, y1 := int(le.Uint16(p[4:])), int(le.Uint16(p[6:])), int(le.Uint16(p[8:])), int(le.Uint16(p[10:]))
			if le.Uint16(p[12:]) != 1 {
				t.Fatal("unexpected target count")
			}
			dx, dy := int(le.Uint16(p[14:])), int(le.Uint16(p[16:]))
			if surfaces[src] == nil || surfaces[dst] == nil || x0 < 0 || y0 < 0 || x1 > w || y1 > h || dx+x1-x0 > w || dy+y1-y0 > h {
				t.Fatal("invalid copy bounds/surface")
			}
			for y := 0; y < y1-y0; y++ {
				copy(surfaces[dst][((dy+y)*w+dx)*4:][:4*(x1-x0)], surfaces[src][((y0+y)*w+x0)*4:][:4*(x1-x0)])
			}
		default:
			t.Fatalf("unexpected PDU %d", kind)
		}
	}
	return surfaces[1]
}

func TestProgressiveSurfaceCopiesOverlap(t *testing.T) {
	const w, h = 192, 128
	old := progressivePixels(w, h, "gradient")
	for _, moves := range [][]gpuconv.SurfaceMove{
		{{SourceX: 3, SourceY: 4, DestinationX: 64, DestinationY: 0, Width: 64, Height: 64}},
		{{SourceX: 0, SourceY: 0, DestinationX: 64, DestinationY: 0, Width: 64, Height: 64}, {SourceX: 64, SourceY: 0, DestinationX: 0, DestinationY: 0, Width: 64, Height: 64}},
		{{SourceX: 0, SourceY: 0, DestinationX: 0, DestinationY: 64, Width: 64, Height: 64}, {SourceX: 64, SourceY: 0, DestinationX: 64, DestinationY: 64, Width: 64, Height: 64}},
	} {
		want := copyProgressiveBaseline(old, w, moves)
		got := replaySurfaceCopies(t, graphicsProgressiveCopies(w, h, moves, true), old, w, h)
		if !bytes.Equal(got, want) {
			t.Fatal("copy order corrupted old sources")
		}
	}
}

func TestProgressiveMoveRefinementSafety(t *testing.T) {
	const w, h = 192, 128
	e := &progressiveEncoder{w: w, h: h, refineBits: 2}
	e.initRefinement(w, h)
	state := jpegcodec.DesktopDamageState{}
	state.Commit(moveDamage(0, 1))
	d := moveDamage(1, 2, jpegcodec.DesktopMove{Source: image.Rect(0, 0, 64, 64), Destination: image.Rect(64, 64, 128, 128)})
	e.pending[4] = true // Destination has a stale unsent UPGRADE.
	for i := range e.coefficients {
		e.coefficients[i] = 123
	}
	moves := e.moveCandidates(d, state, w, h, false)
	if len(moves) != 1 {
		t.Fatal("refined source rejected")
	}
	e.pending[0] = true
	if len(e.moveCandidates(d, state, w, h, false)) != 1 {
		t.Fatal("pending source was not scheduled for priority refinement")
	}
	if got := e.moveSourceRefinements(moves, w, h); !reflect.DeepEqual(got, []bitmapRect{{0, 0, 64, 64}}) {
		t.Fatalf("source refinement selection: %v", got)
	}
	if _, err := e.upgrade(e.moveSourceRefinements(moves, w, h), 2); err != nil {
		t.Fatal(err)
	}
	e.invalidateMovedTiles(moves, w)
	if e.pending[4] {
		t.Fatal("stale destination refinement survived")
	}
	if _, err := e.upgrade([]bitmapRect{{64, 64, 64, 64}}, 3); err == nil {
		t.Fatal("upgraded a destination whose FIRST was not copied")
	}
	for _, v := range e.coefficients[4*progressiveTileCoefficients : 5*progressiveTileCoefficients] {
		if v != 0 {
			t.Fatal("destination coefficients not invalidated")
		}
	}
	if e.coefficients[0] != 123 {
		t.Fatal("source coefficients damaged")
	}
	// The next destination update must start with an absolute FIRST, replacing
	// the decoder's old destination context before any new UPGRADE.
	next := progressivePixels(w, h, "noise")
	if _, err := e.encode(next, w, h, []bitmapRect{{64, 64, 64, 64}}, 4); err != nil {
		t.Fatal(err)
	}
	if !e.pending[4] {
		t.Fatal("new FIRST did not arm refinement")
	}
	if _, err := e.upgrade([]bitmapRect{{64, 64, 64, 64}}, 5); err != nil {
		t.Fatal(err)
	}
}

func TestProgressiveMoveDropsAndMapping(t *testing.T) {
	a := bridge.Frame{Format: capture.FormatRaw, Width: 192, Height: 128, Damage: moveDamage(1, 2, jpegcodec.DesktopMove{Source: image.Rect(0, 0, 64, 64), Destination: image.Rect(64, 64, 128, 128)})}
	b := a
	b.Damage = moveDamage(2, 3)
	merged := mergeProgressiveDamage(a, b)
	if merged.Damage == nil || !merged.Damage.Coalesced || len(merged.Damage.Moves) != 0 || len(merged.Damage.Rects) == 0 {
		t.Fatal("queued move replayed or its endpoint damage lost")
	}
	c := &client{frames: make(chan bridge.Frame, 1)}
	c.frames <- b
	latest, dropped := c.latestCapture(a)
	if dropped != 1 || !latest.Damage.Coalesced || len(latest.Damage.Moves) != 0 {
		t.Fatal("ACK boundary did not downgrade move")
	}
	if a.Damage.Coalesced || len(a.Damage.Moves) != 1 {
		t.Fatal("producer metadata mutated")
	}
	state := jpegcodec.DesktopDamageState{}
	state.Commit(moveDamage(0, 1))
	e := &progressiveEncoder{}
	if len(e.moveCandidates(merged.Damage, state, 192, 128, false)) != 0 {
		t.Fatal("dropped move accepted")
	}
	if len(e.moveCandidates(a.Damage, state, 192, 128, true)) != 0 {
		t.Fatal("refresh accepted old surface")
	}
	mapped := fittedProgressiveDamage(a.Damage, 384, 384)
	if len(mapped.Moves) != 1 || mapped.Moves[0].Destination != image.Rect(128, 192, 256, 320) {
		t.Fatalf("letterbox mapping: %+v", mapped)
	}
}

func TestProgressiveSurfaceMoveShader(t *testing.T) {
	executable := shaderExecutable(t)
	program := startShader(t, executable).(*shaderCompute)
	defer program.Close()
	const w, h = 192, 128
	s := &gpuconv.Surface{SharedTexture: 1, Width: w, Height: h}
	old := progressivePixels(w, h, "gradient")
	moves := []gpuconv.SurfaceMove{{SourceX: 3, SourceY: 4, DestinationX: 64, DestinationY: 0, Width: 64, Height: 64}}
	next := copyProgressiveBaseline(old, w, moves)
	current, flags, err := executeSurfaceMoveShader(program, s, w, h, false, next, old, moves)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(current, next) || flags[1] != 0 {
		t.Fatal("GPU did not compare moved source")
	}
	next[(20*w+70)*4] ^= 128
	_, flags, err = executeSurfaceMoveShader(program, s, w, h, false, next, old, moves)
	if err != nil || flags[1] != 1 {
		t.Fatal("mismatched move accepted", err)
	}
	_, flags, err = executeSurfaceMoveShader(program, s, w, h, true, next, old, moves)
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range flags {
		if flag != 1 {
			t.Fatal("refresh filtered a copied tile")
		}
	}
	// The committed baseline is untouched until frame send succeeds.
	if !reflect.DeepEqual(old, progressivePixels(w, h, "gradient")) {
		t.Fatal("comparison mutated the old GPU baseline")
	}
}

func TestProgressiveSurfaceMoveBitmapLoop(t *testing.T) {
	executable := shaderExecutable(t)
	program := startShader(t, executable).(*shaderCompute)
	const w, h = 192, 128
	a := &gpuconv.Surface{SharedTexture: 1, Width: w, Height: h}
	b := &gpuconv.Surface{SharedTexture: 2, Width: w, Height: h}
	last := &gpuconv.Surface{SharedTexture: 3, Width: w, Height: h}
	old := progressivePixels(w, h, "gradient")
	move := gpuconv.SurfaceMove{SourceX: 3, SourceY: 4, DestinationX: 64, DestinationY: 0, Width: 64, Height: 64}
	next := copyProgressiveBaseline(old, w, []gpuconv.SurfaceMove{move})
	changed := bytes.Clone(next)
	changed[(20*w+70)*4] ^= 128
	fixture := &surfaceComputeFixture{program: program, frames: map[*gpuconv.Surface][]byte{a: old, b: next, last: changed}, committed: make(chan struct{}, 8)}
	encoder := &progressiveEncoder{native: &gpuProgressiveCodec{compute: fixture, quantization: 6}, required: true, first: true, quantization: 6}
	c := channelTestClient()
	c.codec = codecProgressive
	c.sent = 0
	c.suspendAck = true
	conn := &surfaceTestConn{frames: make(chan struct{}, 8)}
	c.conn = conn
	c.frames = make(chan bridge.Frame, 1)
	finished := make(chan error, 1)
	go func() { finished <- c.bitmapLoopWithEncoder(encoder) }()
	defer func() {
		close(c.done)
		if err := <-finished; err != nil {
			t.Error(err)
		}
	}()
	send := func(surface *gpuconv.Surface, d *jpegcodec.DesktopDamage) {
		t.Helper()
		c.frames <- bridge.Frame{Format: capture.FormatSurface, Width: w, Height: h, Surface: surface, Buffer: framebuf.New(nil, nil), Damage: d}
		select {
		case <-fixture.committed:
		case err := <-finished:
			finished <- err
			t.Fatal("bitmap loop failed", err)
		case <-time.After(5 * time.Second):
			t.Fatal("bitmap loop did not commit")
		}
	}
	send(a, moveDamage(0, 1))
	firstEncodes := fixture.resident.Load()
	send(b, moveDamage(1, 2, jpegcodec.DesktopMove{Source: image.Rect(3, 4, 67, 68), Destination: image.Rect(64, 0, 128, 64)}))
	if conn.copyCommands != 2 || fixture.resident.Load() != firstEncodes {
		t.Fatal("pure surface copy encoded pixels or failed snapshot copy")
	}
	send(last, moveDamage(2, 3))
	if fixture.resident.Load() != firstEncodes+1 || fixture.uploads.Load() != 0 {
		t.Fatal("copied destination change lost or full pixels uploaded")
	}
	// Layout reset must delete the scratch ID before a later copy recreates it.
	c.scaledMapping = true
	conn.allowScaled = true
	c.setOutputSize(384, 256)
	send(last, moveDamage(3, 4))
	if _, exists := conn.surfaces[progressiveMoveScratch]; exists {
		t.Fatal("layout change retained stale scratch surface")
	}
}

func TestProgressiveMoveProducerQueue(t *testing.T) {
	c := channelTestClient()
	c.codec = codecProgressive
	c.frames = make(chan bridge.Frame, 1)
	c.s.active = c
	a := bridge.Frame{Format: capture.FormatRaw, Width: 192, Height: 128, Data: make([]byte, 192*128*4), Damage: moveDamage(1, 2, jpegcodec.DesktopMove{Source: image.Rect(0, 0, 64, 64), Destination: image.Rect(64, 64, 128, 128)})}
	b := a
	b.Damage = moveDamage(2, 3)
	c.s.PushFrame(a)
	c.s.PushFrame(b)
	latest := <-c.frames
	defer latest.Release()
	if c.lastSource != nil {
		defer c.lastSource.Release()
	}
	if !latest.Damage.Coalesced || len(latest.Damage.Moves) != 0 || len(latest.Damage.Rects) == 0 {
		t.Fatal("producer queue replayed discarded move")
	}
}

func TestProgressiveMoveGPURefinementCache(t *testing.T) {
	executable := shaderExecutable(t)
	program := startShader(t, executable).(*shaderCompute)
	const w, h = 192, 128
	a := &gpuconv.Surface{SharedTexture: 1, Width: w, Height: h}
	b := &gpuconv.Surface{SharedTexture: 2, Width: w, Height: h}
	old := progressivePixels(w, h, "gradient")
	fixture := &surfaceComputeFixture{program: program, frames: map[*gpuconv.Surface][]byte{a: old}}
	e := &progressiveEncoder{native: &gpuProgressiveCodec{compute: fixture, quantization: 6}, required: true, first: true, quantization: 6, refineBits: 2}
	defer e.close()
	rects, err := e.prepareSurface(a, w, h, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.encodeSurface(w, h, rects, 1); err != nil {
		t.Fatal(err)
	}
	if err = e.commitSurface(); err != nil {
		t.Fatal(err)
	}
	if _, err = e.upgrade(e.pendingRects(8), 2); err != nil {
		t.Fatal(err)
	}
	state := jpegcodec.DesktopDamageState{}
	state.Commit(moveDamage(0, 1))
	move := jpegcodec.DesktopMove{Source: image.Rect(0, 0, 64, 64), Destination: image.Rect(64, 64, 128, 128)}
	// Retain an old destination FIRST task to model the stale UPGRADE hazard.
	e.pending[4] = true
	candidates := e.moveCandidates(moveDamage(1, 2, move), state, w, h, false)
	next := copyProgressiveBaseline(old, w, candidates)
	fixture.frames[b] = next
	rects, copies, err := e.prepareSurfaceMoves(b, w, h, false, candidates)
	if err != nil || len(rects) != 0 || len(copies) != 1 {
		t.Fatal("copy not verified", err, rects, copies)
	}
	e.invalidateMovedTiles(copies, w)
	if _, err = e.upgrade([]bitmapRect{{64, 64, 64, 64}}, 3); err == nil {
		t.Fatal("GPU reused stale destination cache after copy")
	}
	if err = e.commitSurface(); err != nil {
		t.Fatal(err)
	}
	next[(70*w+70)*4] ^= 128
	rects, err = e.prepareSurface(b, w, h, false)
	if err != nil {
		t.Fatal(err)
	}
	first, err := e.encodeSurface(w, h, rects, 4)
	if err != nil {
		t.Fatal(err)
	}
	upgrade, err := e.upgrade(rects, 5)
	if err != nil {
		t.Fatal(err)
	}
	cpu := &progressiveEncoder{w: w, h: h, quantization: 6, refineBits: 2}
	cpu.initRefinement(w, h)
	wantFirst, err := cpu.encode(next, w, h, rects, 4)
	if err != nil {
		t.Fatal(err)
	}
	wantUpgrade, err := cpu.upgrade(rects, 5)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, wantFirst) || !bytes.Equal(upgrade, wantUpgrade) {
		t.Fatal("copied tile's new FIRST/UPGRADE reused old GPU coefficients")
	}
}

// A fresh capture is queued before frame 1 finishes, preventing idle refinement.
// Frame 2 must refine the old unaligned source before snapshot/copy, then encode
// a changed source tile using its new coefficients. Exercise both cache owners.
func TestProgressiveMovePriorityRefinement(t *testing.T) {
	for _, gpu := range []bool{false, true} {
		t.Run(map[bool]string{false: "cpu", true: "gpu"}[gpu], func(t *testing.T) {
			const w, h = 192, 128
			old := progressivePixels(w, h, "gradient")
			move := gpuconv.SurfaceMove{SourceX: 3, SourceY: 4, DestinationX: 64, DestinationY: 0, Width: 64, Height: 64}
			next := copyProgressiveBaseline(old, w, []gpuconv.SurfaceMove{move})
			next[0] ^= 128 // Source tile is replaced after its old UPGRADE.
			sourceRects := []bitmapRect{{0, 0, 64, 64}, {64, 0, 64, 64}, {0, 64, 64, 64}, {64, 64, 64, 64}}
			oracle := &progressiveEncoder{first: true, quantization: 6, refineBits: 2}
			defer oracle.close()
			if _, err := oracle.encode(old, w, h, dirtyProgressiveRectsFormat(old, nil, w, h, progressiveBGRA), 1); err != nil {
				t.Fatal(err)
			}
			wantUpgrade, err := oracle.finishUpgrades(sourceRects, 2)
			if err != nil {
				t.Fatal(err)
			}
			encoder := &progressiveEncoder{first: true, quantization: 6, refineBits: 2}
			first := bridge.Frame{Format: capture.FormatRaw, Width: w, Height: h, Data: old, Damage: moveDamage(0, 1)}
			second := bridge.Frame{Format: capture.FormatRaw, Width: w, Height: h, Data: next, Damage: moveDamage(1, 2, jpegcodec.DesktopMove{Source: image.Rect(3, 4, 67, 68), Destination: image.Rect(64, 0, 128, 64)})}
			var fixture *surfaceComputeFixture
			if gpu {
				program := startShader(t, shaderExecutable(t)).(*shaderCompute)
				a := &gpuconv.Surface{SharedTexture: 1, Width: w, Height: h}
				b := &gpuconv.Surface{SharedTexture: 2, Width: w, Height: h}
				fixture = &surfaceComputeFixture{program: program, frames: map[*gpuconv.Surface][]byte{a: old, b: next}}
				encoder.native = &gpuProgressiveCodec{compute: fixture, quantization: 6}
				encoder.required = true
				first.Format, first.Surface, first.Data = capture.FormatSurface, a, nil
				second.Format, second.Surface, second.Data = capture.FormatSurface, b, nil
				first.Buffer, second.Buffer = framebuf.New(nil, nil), framebuf.New(nil, nil)
			}
			c := channelTestClient()
			c.codec, c.sent, c.suspendAck = codecProgressive, 0, true
			c.s.cfg.ProgressiveRefinement = 2
			c.frames = make(chan bridge.Frame, 1)
			conn := &surfaceTestConn{frames: make(chan struct{}, 4)}
			c.conn = conn
			var frame uint32
			var events []uint16
			var payloads [][]byte
			conn.onGraphics = func(kind uint16, body []byte) {
				if kind == 0xb {
					frame = le.Uint32(body[4:])
				}
				if frame == 2 && (kind == 2 || kind == 5) {
					events = append(events, kind)
					if kind == 2 {
						payloads = append(payloads, bytes.Clone(body[13:]))
					}
				}
				if kind == 0xc {
					if frame == 1 {
						c.frames <- second
					} else {
						close(c.done)
					}
				}
			}
			c.frames <- first
			finished := make(chan error, 1)
			go func() { finished <- c.bitmapLoopWithEncoder(encoder) }()
			select {
			case err := <-finished:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("priority refinement stalled")
			}
			if len(events) < 5 || len(events) > 6 || !reflect.DeepEqual(events[:5], []uint16{2, 2, 5, 5, 2}) || len(events) == 6 && events[5] != 2 {
				t.Fatalf("UPGRADE/snapshot/copy/FIRST order: %v", events)
			}
			if len(events) == 6 {
				for _, tile := range progressiveCaptureTiles(t, payloads[3]) {
					if tile.kind != 0xccc7 || tile.x != 2 {
						t.Fatal("optional refinement touched the changed source or copied destination")
					}
				}
			}
			if !bytes.Equal(payloads[0], wantUpgrade[0]) || !bytes.Equal(payloads[1], wantUpgrade[1]) {
				t.Fatal("source UPGRADE did not use the old coefficients or tiles were not deduplicated")
			}
			if fixture != nil && (fixture.resident.Load() != 2 || fixture.uploads.Load() != 0) {
				t.Fatal("move re-encoded destination or uploaded GPU pixels")
			}
		})
	}
}

func TestProgressiveMoveSourceRefinementUnion(t *testing.T) {
	e := &progressiveEncoder{w: 192, h: 128, refineBits: 2}
	e.initRefinement(192, 128)
	for i := range e.pending {
		e.pending[i] = true
	}
	moves := []gpuconv.SurfaceMove{
		{SourceX: 3, SourceY: 4, DestinationX: 64, DestinationY: 0, Width: 64, Height: 64},
		{SourceX: 3, SourceY: 4, DestinationX: 128, DestinationY: 0, Width: 64, Height: 64},
	}
	want := []bitmapRect{{0, 0, 64, 64}, {64, 0, 64, 64}, {0, 64, 64, 64}, {64, 64, 64, 64}}
	if got := e.moveSourceRefinements(moves, 192, 128); !reflect.DeepEqual(got, want) {
		t.Fatalf("overlapping sources not deduplicated: %v", got)
	}
	if got := e.moveSourceRefinements(nil, 192, 128); len(got) != 0 {
		t.Fatal("refined unverified moves")
	}
}
