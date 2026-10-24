package rdp

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
	"anykvm/internal/framebuf"
	"anykvm/internal/gpuconv"
)

type progressiveCaptureTile struct {
	kind          uint16
	quality, x, y int
	data          []byte
}

type arrivingCaptureCompute struct {
	progressiveCompute
	after func()
}

func (f *arrivingCaptureCompute) Run(p []byte, m []uint32) ([]byte, [48]uint32, error) {
	out, lengths, err := f.progressiveCompute.Run(p, m)
	if err == nil && m[4] == 2 {
		f.after()
	}
	return out, lengths, err
}

type arrivingCaptureSurfaceCompute struct {
	*surfaceComputeFixture
	after func()
}

func (f *arrivingCaptureSurfaceCompute) Run(p []byte, m []uint32) ([]byte, [48]uint32, error) {
	out, lengths, err := f.surfaceComputeFixture.Run(p, m)
	if err == nil && m[4] == 2 {
		f.after()
	}
	return out, lengths, err
}

func progressiveCaptureTiles(t *testing.T, packet []byte) []progressiveCaptureTile {
	t.Helper()
	var out []progressiveCaptureTile
	for len(packet) >= 6 {
		n := int(le.Uint32(packet[2:]))
		if n < 6 || n > len(packet) {
			t.Fatal("invalid Progressive block")
		}
		if le.Uint16(packet) == 0xccc4 {
			offset := 18 + 8*int(le.Uint16(packet[7:])) + 5*int(packet[9])
			quality := 0
			if packet[10] > 0 {
				quality = int(packet[offset])
			}
			tiles := packet[offset+16*int(packet[10]) : n]
			for len(tiles) >= 6 {
				size := int(le.Uint32(tiles[2:]))
				if size < 14 || size > len(tiles) {
					t.Fatal("invalid Progressive tile")
				}
				out = append(out, progressiveCaptureTile{le.Uint16(tiles), quality, int(le.Uint16(tiles[9:])), int(le.Uint16(tiles[11:])), bytes.Clone(tiles[:size])})
				tiles = tiles[size:]
			}
		}
		packet = packet[n:]
	}
	return out
}

// Keep a capture queued even inside every transport write. This reproduces a
// source which never goes idle, independently of goroutine timing or sleeps.
func TestProgressiveContinuousCaptureTileProgress(t *testing.T) {
	for _, backend := range []string{"go", "native-scalar", "gpu", "gpu-surface"} {
		t.Run(backend, func(t *testing.T) {
			executable := ""
			if backend == "gpu" || backend == "gpu-surface" {
				executable = shaderExecutable(t)
			}
			for _, changing := range []bool{false, true} {
				t.Run(fmt.Sprintf("changing=%t", changing), func(t *testing.T) {
					const w, h = 192, 64
					e := &progressiveEncoder{first: true, quantization: 6, required: true}
					var fixture *surfaceComputeFixture
					if backend == "native-scalar" {
						var err error
						e.native, err = newNativeProgressive(backend, 6)
						if errors.Is(err, errProgressiveNativeUnavailable) {
							t.Skip(err)
						}
						if err != nil {
							t.Fatal(err)
						}
					} else if backend == "gpu" {
						e.native = &gpuProgressiveCodec{compute: startShader(t, executable), quantization: 6}
					} else if backend == "gpu-surface" {
						fixture = &surfaceComputeFixture{program: startShader(t, executable).(*shaderCompute), frames: make(map[*gpuconv.Surface][]byte)}
						e.native = &gpuProgressiveCodec{compute: fixture, quantization: 6}
					}
					c := channelTestClient()
					c.codec, c.sent = codecProgressive, 0
					c.s.cfg.ProgressiveRefinement = 2
					c.frames = make(chan bridge.Frame, 2)
					base := progressivePixels(w, h, "noise")
					sequence := 0
					push := func() {
						pixels := bytes.Clone(base)
						pixels[3] = byte(sequence) // alpha is not a visible tile change
						if changing {
							pixels[0] = byte(sequence * 7)
						}
						f := bridge.Frame{Format: capture.FormatRaw, Width: w, Height: h, Data: pixels, CaptureFPS: 10}
						if fixture != nil {
							s := &gpuconv.Surface{SharedTexture: uintptr(sequence + 1), Width: w, Height: h}
							fixture.frames[s] = pixels
							f.Format, f.Data, f.Surface, f.Buffer = capture.FormatSurface, nil, s, framebuf.New(nil, nil)
						}
						sequence++
						c.frames <- f
					}
					arrive := func() {
						if len(c.frames) < cap(c.frames) {
							push()
						}
					}
					if g, ok := e.native.(*gpuProgressiveCodec); ok {
						if fixture != nil {
							g.compute = &arrivingCaptureSurfaceCompute{fixture, arrive}
						} else {
							g.compute = &arrivingCaptureCompute{g.compute, arrive}
						}
					} else {
						e.native = &interleavedProgressiveCodec{nativeProgressiveCodec: e.native, afterUpgrade: arrive}
					}
					var qualities, firsts [3]int
					frames := 0
					var problem string
					conn := &surfaceTestConn{frames: make(chan struct{}, 64)}
					c.conn = conn
					conn.onGraphics = func(kind uint16, body []byte) {
						if kind == 2 {
							for _, tile := range progressiveCaptureTiles(t, body[13:]) {
								i := tile.x
								if i < 0 || i >= 3 || tile.y != 0 {
									problem = "invalid tile coordinates"
									continue
								}
								if tile.kind == 0xccc6 {
									firsts[i]++
									if firsts[i] > 1 && (!changing || i != 0) {
										problem = "FIRST resent for unchanged tile"
									}
									qualities[i] = 50
									if firsts[0] > 1 && i == 0 && tile.quality != 50 {
										problem = "changed tile did not restart at 50%"
									}
								} else if tile.kind == 0xccc7 {
									if len(c.frames) == 0 {
										problem = "quality progress relied on an empty capture queue"
									}
									if changing && i == 0 {
										problem = "UPGRADE consumed time for a tile changed in this capture"
									}
									want := 75
									if qualities[i] == 75 {
										want = 100
									}
									if tile.quality != want || qualities[i] == 100 {
										problem = "UPGRADE sign/precision history restarted or skipped a pass"
									}
									qualities[i] = tile.quality
								}
							}
						}
						if kind == 0xc {
							frames++
							if err := c.acknowledgeFrame(le.Uint32(body), 0); err != nil {
								problem = err.Error()
							}
							complete := qualities[1] == 100 && qualities[2] == 100 && (changing || qualities[0] == 100)
							if problem != "" || changing && frames >= 12 || !changing && complete || frames >= 16 {
								close(c.done)
								return
							}
							for len(c.frames) < cap(c.frames) {
								push()
							}
						}
					}
					push()
					push()
					finished := make(chan error, 1)
					go func() { finished <- c.bitmapLoopWithEncoder(e) }()
					select {
					case err := <-finished:
						if err != nil {
							t.Fatal(err)
						}
					case <-time.After(10 * time.Second):
						close(c.done)
						<-finished
						t.Fatal("continuous captures starved refinement")
					}
					for len(c.frames) > 0 {
						f := <-c.frames
						f.Release()
					}
					if problem != "" {
						t.Fatal(problem)
					}
					if qualities[1] != 100 || qualities[2] != 100 || !changing && qualities[0] != 100 {
						t.Fatalf("static tiles never completed: %v", qualities)
					}
					if changing && firsts[0] != 12 {
						t.Fatalf("moving tile output lost a FIRST/ACK opportunity: firsts=%v frames=%d", firsts, frames)
					}
					if firsts[1] != 1 || firsts[2] != 1 {
						t.Fatalf("static FIRSTs restarted: %v", firsts)
					}
					if fixture != nil && fixture.uploads.Load() != 0 {
						t.Fatal("resident refinement uploaded CPU pixels")
					}
				})
			}
		})
	}
}

func TestProgressiveCaptureRefinementBudget(t *testing.T) {
	e := &progressiveEncoder{first: true, quantization: 6, refineBits: 2}
	defer e.close()
	const w, h = 128, 64
	rects := []bitmapRect{{0, 0, 64, 64}, {64, 0, 64, 64}}
	if _, err := e.encode(progressivePixels(w, h, "noise"), w, h, rects, 1); err != nil {
		t.Fatal(err)
	}
	c := channelTestClient()
	c.setGraphicsFPS(60)
	id := c.nextGraphicsFrame()
	for _, exhausted := range []bool{false, true} {
		started := time.Now()
		if exhausted {
			started = started.Add(-time.Second)
		}
		selected, packet, err := c.prepareCaptureRefinement(e, rects[:1], nil, id, started, 16)
		if err != nil {
			t.Fatal(err)
		}
		if exhausted && (len(selected) != 0 || len(packet) != 0) {
			t.Fatal("refinement exceeded a spent source frame budget")
		}
		if !exhausted && (len(selected) != 1 || selected[0] != rects[1]) {
			t.Fatal("refinement did not exclude the changed tile")
		}
		if len(c.pendingFrames) != 1 || c.sent != id || e.tileStage(1) != 1 {
			t.Fatal("prepared refinement used another ACK frame or advanced before write")
		}
	}
	e.upgradeTileCost, e.upgradeCostAt = time.Second, time.Now()
	if selected, _, err := c.prepareCaptureRefinement(e, rects[:1], nil, id, time.Now(), 16); err != nil || len(selected) != 0 {
		t.Fatal("recent expensive encoding estimate did not protect source frame time")
	}
	e.upgradeCostAt = time.Now().Add(-time.Second)
	if selected, _, err := c.prepareCaptureRefinement(e, rects[:1], nil, id, time.Now(), 16); err != nil || len(selected) != 1 {
		t.Fatal("an old encoding stall permanently prevented static tile completion")
	}
	e.discardRefinements()
	before := c.sent
	if n, _, _, err := c.sendCheckedCaptureRefinement(e, time.Now()); err != nil || n != 0 || c.sent != before {
		t.Fatal("completed static captures kept reserving unused frame IDs")
	}
}

func TestProgressiveChangingTileDoesNotStarveFinalPass(t *testing.T) {
	e := &progressiveEncoder{w: 192, h: 64, pending: []bool{true, true, true}, stages: []uint8{1, 2, 2}}
	for range 6 {
		// A new FIRST repeatedly resets only the changing tile's stage.
		e.pending[0], e.stages[0] = true, 1
		batch := e.pendingRects(1)
		if len(batch) != 1 {
			t.Fatal("pending tile lost")
		}
		e.commitUpgrade(batch)
	}
	if e.pending[1] || e.pending[2] {
		t.Fatal("repeated FIRSTs starved static neighbors at 75%")
	}
}

func TestProgressiveCheckedCaptureRechecksACKCapacity(t *testing.T) {
	e := &progressiveEncoder{first: true, quantization: 6, refineBits: 2}
	defer e.close()
	if _, err := e.encode(progressivePixels(64, 64, "noise"), 64, 64, []bitmapRect{{0, 0, 64, 64}}, 1); err != nil {
		t.Fatal(err)
	}
	c := channelTestClient()
	c.setGraphicsFPS(60)
	c.graphicsFlow.rtt, c.graphicsFlow.baseline = 100*time.Millisecond, 100*time.Millisecond
	old := c.nextGraphicsFrame()
	e.native = &interleavedProgressiveCodec{afterUpgrade: func() {
		c.ackMu.Lock()
		c.graphicsFlow.rtt, c.graphicsFlow.baseline, c.graphicsFlow.variation = time.Millisecond, time.Millisecond, 0
		c.graphicsFlow.windowLimit = 2
		c.ackMu.Unlock()
	}}
	n, _, _, err := c.sendCheckedCaptureRefinement(e, time.Now())
	if err != nil || n != 0 || len(c.pendingFrames) != 1 || !e.pending[0] || e.tileStage(0) != 1 {
		t.Fatal("duplicate-capture refinement consumed a newly reserved fresh-frame slot")
	}
	if _, ok := c.pendingFrames[old]; !ok {
		t.Fatal("preemption released the wrong frame reservation")
	}
}
