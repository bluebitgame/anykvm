package rdp

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"anykvm/internal/bridge"
	"anykvm/internal/gpuconv"
)

type interleavedProgressiveCodec struct {
	nativeProgressiveCodec
	afterUpgrade func()
}

func (e *interleavedProgressiveCodec) name() string {
	if e.nativeProgressiveCodec == nil {
		return "Go (interleaved fixture)"
	}
	return e.nativeProgressiveCodec.name()
}

func (e *interleavedProgressiveCodec) close() {
	if e.nativeProgressiveCodec != nil {
		e.nativeProgressiveCodec.close()
	}
}

func (e *interleavedProgressiveCodec) encodePass(pixels []byte, w, h int, rects []bitmapRect, frame uint32, first bool, format progressivePixelFormat, bits int, upgrade bool, cache []int32) ([]byte, error) {
	var out []byte
	var err error
	if e.nativeProgressiveCodec == nil {
		out = encodeProgressivePass(pixels, w, h, rects, frame, first, 6, format, bits, upgrade, cache)
	} else {
		out, err = e.nativeProgressiveCodec.(nativeProgressiveLayered).encodePass(pixels, w, h, rects, frame, first, format, bits, upgrade, cache)
	}
	if upgrade && err == nil && e.afterUpgrade != nil {
		e.afterUpgrade()
	}
	return out, err
}

// Interleave feedback/capture with actual CPU or shipped-HLSL entropy work.
// Discarded 75% and 100% packets must leave identical sign/precision history,
// including the unchanged neighbor after a new FIRST replaces only one tile.
func TestProgressiveRefinementPreemptionAndReplacement(t *testing.T) {
	for _, backend := range []string{"go", "native-scalar", "gpu", "gpu-surface"} {
		t.Run(backend, func(t *testing.T) {
			executable := ""
			if backend == "gpu" || backend == "gpu-surface" {
				executable = shaderExecutable(t)
			}
			for _, stage := range []int{75, 100} {
				for _, reason := range []string{"capture", "refresh", "window"} {
					t.Run(fmt.Sprintf("%d/%s", stage, reason), func(t *testing.T) {
						e := &progressiveEncoder{first: true, quantization: 6, refineBits: 2, required: true}
						defer e.close()
						const w, h = 128, 64
						pixels := progressivePixels(w, h, "noise")
						var surfaceFixture *surfaceComputeFixture
						surface := &gpuconv.Surface{SharedTexture: 1, Width: w, Height: h}
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
							surfaceFixture = &surfaceComputeFixture{program: startShader(t, executable).(*shaderCompute), frames: make(map[*gpuconv.Surface][]byte)}
							e.native = &gpuProgressiveCodec{compute: surfaceFixture, quantization: 6}
						}
						encode := func(encoder *progressiveEncoder, rects []bitmapRect, frame uint32) ([]byte, error) {
							if encoder == e && surfaceFixture != nil {
								surfaceFixture.frames[surface] = append([]byte(nil), pixels...)
								dirty, err := e.prepareSurface(surface, w, h, false)
								if err != nil {
									return nil, err
								}
								if !reflect.DeepEqual(dirty, rects) {
									t.Fatalf("resident dirty tiles=%v, expected %v", dirty, rects)
								}
								out, err := e.encodeSurface(w, h, rects, frame)
								if err == nil {
									err = e.commitSurface()
								}
								return out, err
							}
							return encoder.encode(pixels, w, h, rects, frame)
						}
						oracle := &progressiveEncoder{first: true, quantization: 6, refineBits: 2}
						defer oracle.close()
						rects := []bitmapRect{{0, 0, 64, 64}, {64, 0, 64, 64}}
						for _, encoder := range []*progressiveEncoder{e, oracle} {
							if _, err := encode(encoder, rects, 1); err != nil {
								t.Fatal(err)
							}
							if stage == 100 {
								if _, err := encoder.upgrade(rects, 2); err != nil {
									t.Fatal(err)
								}
							}
						}
						conn := &dvcRecordingConn{}
						c := channelTestClient()
						c.conn, c.frames = conn, make(chan bridge.Frame, 1)
						c.graphicsFlow = graphicsFlowControl{interval: time.Second / 60, baseline: 50 * time.Millisecond, rtt: 50 * time.Millisecond}
						original := e.native
						e.native = &interleavedProgressiveCodec{nativeProgressiveCodec: original, afterUpgrade: func() {
							switch reason {
							case "capture":
								c.frames <- bridge.Frame{}
							case "refresh":
								c.reset.Store(true)
							case "window":
								c.ackMu.Lock()
								c.graphicsFlow.recoverUntil = time.Now().Add(time.Second)
								c.ackMu.Unlock()
							}
						}}
						before := append([]uint8(nil), e.stages...)
						n, payload, _, _, err := c.sendProgressiveRefinement(e, time.Now())
						e.native = original
						if err != nil || n != 0 || payload != 0 || conn.data.Len() != 0 || len(c.pendingFrames) != 0 || !reflect.DeepEqual(before, e.stages) {
							t.Fatalf("preempted packet was sent/committed or retained an ACK slot: n=%d bytes=%d err=%v", n, payload, err)
						}
						select {
						case <-c.frames:
						default:
						}
						c.reset.Store(false)
						c.graphicsFlow.recoverUntil = time.Time{}
						// Replace one tile; retain its neighbor's previous stage/cache.
						pixels[64*4] ^= 255
						got, err := encode(e, rects[1:], 10)
						want, expectedErr := encode(oracle, rects[1:], 10)
						if err != nil || expectedErr != nil || !bytes.Equal(got, want) || !reflect.DeepEqual(e.stages, oracle.stages) {
							t.Fatal("replacement FIRST retained canceled precision/cache")
						}
						for len(e.pendingRects(8)) != 0 {
							batch := oracle.pendingRects(8)
							id, stamp := c.sent+1, time.Now()
							encoded, err := oracle.upgrade(batch, id)
							if err != nil {
								t.Fatal(err)
							}
							want := appendGraphicsProgressive(graphicsStartFrame(id, stamp), encoded)
							want = append(want, gfxPDU(0xc, u32(id))...)
							n, _, _, _, err := c.sendProgressiveRefinement(e, stamp)
							if err != nil || n != len(batch) {
								t.Fatalf("replacement did not resume refinement: n=%d err=%v", n, err)
							}
							if !bytes.Equal(recordedRefinementFrame(t, conn), want) || !reflect.DeepEqual(e.stages, oracle.stages) {
								t.Fatal("UPGRADE differs from the fresh CPU history after preemption")
							}
							if err := c.acknowledgeFrame(id, 0); err != nil {
								t.Fatal(err)
							}
						}
						if surfaceFixture != nil && (surfaceFixture.uploads.Load() != 0 || surfaceFixture.resident.Load() != 2) {
							t.Fatal("resident replacement/refinement used CPU pixels or lost the GPU cache")
						}
					})
				}
			}
		})
	}
}

func recordedRefinementFrame(t *testing.T, conn *dvcRecordingConn) []byte {
	t.Helper()
	var compressed []byte
	total := -1
	for i, p := range recordedDVC(t, conn) {
		if i == 0 && len(p) >= 6 && p[0] == 0x28 {
			total = int(le.Uint32(p[2:]))
			p = p[6:]
		} else if len(p) >= 2 && p[0] == 0x30 {
			p = p[2:]
		} else {
			t.Fatal("invalid refinement DVC fragment")
		}
		compressed = append(compressed, p...)
	}
	if total >= 0 && len(compressed) != total {
		t.Fatal("incomplete refinement frame")
	}
	var decoder zgfxDecoder
	frame, err := decoder.decode(compressed)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func TestProgressiveRefinementKeepsCongestionProbeAndByteHeadroom(t *testing.T) {
	now := time.Now()
	c := &client{graphicsFlow: graphicsFlowControl{interval: time.Second / 60, rtt: 50 * time.Millisecond, baseline: 50 * time.Millisecond, recoverUntil: now.Add(time.Second)}}
	if !c.refinementWindowFull() || c.frameWindowFull() {
		t.Fatal("background work consumed the fresh capture's congestion probe")
	}
	c.graphicsFlow.recoverUntil = time.Time{}
	c.graphicsFlow.frameBytes = 1 << 20
	id := c.nextGraphicsFrame()
	c.pendingFrames[id] = graphicsPendingFrame{bytes: graphicsByteBudget - (512 << 10)}
	if !c.refinementWindowFull() || c.frameWindowFull() {
		t.Fatal("background work consumed the fresh capture's byte headroom")
	}
}

type rejectedRefinementConn struct{ displayActivationConn }

func (rejectedRefinementConn) Write([]byte) (int, error) {
	return 0, errors.New("refinement transport rejected")
}

func TestProgressiveRefinementWriteFailureKeepsStage(t *testing.T) {
	for _, stage := range []int{75, 100} {
		t.Run(fmt.Sprint(stage), func(t *testing.T) {
			e := &progressiveEncoder{first: true, quantization: 6, refineBits: 2}
			defer e.close()
			rects := []bitmapRect{{0, 0, 64, 64}}
			if _, err := e.encode(progressivePixels(64, 64, "noise"), 64, 64, rects, 1); err != nil {
				t.Fatal(err)
			}
			if stage == 100 {
				if _, err := e.upgrade(rects, 2); err != nil {
					t.Fatal(err)
				}
			}
			before := e.tileStage(0)
			c := channelTestClient()
			c.conn = rejectedRefinementConn{}
			n, payload, _, _, err := c.sendProgressiveRefinement(e, time.Now())
			if err == nil || n != 0 || payload != 0 || e.tileStage(0) != before || !e.pending[0] || len(c.pendingFrames) != 0 {
				t.Fatal("failed UPGRADE write committed precision or retained an ACK reservation")
			}
		})
	}
}
