package rdp

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

type parallelTestCodec struct {
	encodeWork func([]byte, []bitmapRect, bool, []int32) ([]byte, error)
	active     atomic.Bool
	closed     atomic.Bool
	serial     atomic.Int32
}

func (c *parallelTestCodec) name() string { return "test tiles" }
func (c *parallelTestCodec) close()       { c.closed.Store(true) }
func (c *parallelTestCodec) encode(p []byte, w, h int, r []bitmapRect, f uint32, first bool, format progressivePixelFormat) ([]byte, error) {
	return c.encodePass(p, w, h, r, f, first, format, 0, false, nil)
}
func (c *parallelTestCodec) encodePass(p []byte, w, h int, r []bitmapRect, f uint32, first bool, format progressivePixelFormat, bits int, upgrade bool, cache []int32) ([]byte, error) {
	c.serial.Add(1)
	data, err := c.encodeTiles(p, w, h, r, format, bits, upgrade, cache)
	if err != nil {
		return nil, err
	}
	count := 0
	for _, rect := range r {
		count += progressiveRectTiles(rect)
	}
	return progressiveWire(r, f, first, 6, bits, upgrade, data, count, bits != 0), nil
}
func (c *parallelTestCodec) encodeTiles(p []byte, w, h int, r []bitmapRect, format progressivePixelFormat, bits int, upgrade bool, cache []int32) ([]byte, error) {
	if c.closed.Load() || !c.active.CompareAndSwap(false, true) {
		return nil, errors.New("codec reused while busy or closed")
	}
	defer c.active.Store(false)
	return c.encodeWork(p, r, upgrade, cache)
}

func parallelTestRects() []bitmapRect {
	var rects []bitmapRect
	for x := 0; x < 512; x += 64 {
		rects = append(rects, bitmapRect{x, 0, 64, 64})
	}
	return rects
}

func TestProgressiveParallelOrderAndCache(t *testing.T) {
	started := make(chan int, 4)
	finished := make(chan int, 4)
	release := [4]chan struct{}{}
	for i := range release {
		release[i] = make(chan struct{})
	}
	work := func(p []byte, rects []bitmapRect, upgrade bool, cache []int32) ([]byte, error) {
		group := rects[0].x / 128
		started <- group
		<-release[group]
		var data []byte
		for _, r := range rects {
			index := r.x / 64
			if !upgrade {
				cache[index*progressiveTileCoefficients] = int32(p[0])
			}
			data = append(data, byte(index), byte(cache[index*progressiveTileCoefficients]))
		}
		finished <- group
		return data, nil
	}
	makeCodec := func() (progressiveTileCodec, error) { return &parallelTestCodec{encodeWork: work}, nil }
	first, _ := makeCodec()
	pool := newParallelProgressive(first, 6, 4, makeCodec)
	defer pool.close()
	cache := make([]int32, 8*progressiveTileCoefficients)
	pixels := make([]byte, 512*64*4)
	pixels[0] = 7
	rects := parallelTestRects()
	for _, upgrade := range []bool{false, true} {
		returned := make(chan progressiveTileResult, 1)
		go func() {
			data, err := pool.encodePass(pixels, 512, 64, rects, 17, true, progressiveBGRA, 2, upgrade, cache)
			returned <- progressiveTileResult{data, err}
		}()
		for range 4 {
			<-started
		}
		for i := 3; i >= 0; i-- {
			close(release[i])
			if got := <-finished; got != i {
				t.Fatalf("completion=%d, want %d", got, i)
			}
		}
		got := <-returned
		var payload []byte
		for i := 0; i < 8; i++ {
			payload = append(payload, byte(i), 7)
		}
		want := progressiveLayeredWire(rects, 17, true, 6, 2, upgrade, payload, 8)
		if got.err != nil || !bytes.Equal(got.data, want) {
			t.Fatalf("upgrade=%v: completion order changed output: %v", upgrade, got.err)
		}
		for i := range release {
			release[i] = make(chan struct{})
		}
		// UPGRADE must use FIRST's values despite a different current capture.
		pixels[0] = 11
	}
	for i := 0; i < 8; i++ {
		if cache[i*progressiveTileCoefficients] != 7 {
			t.Fatal("UPGRADE changed FIRST cache")
		}
	}
}

func TestProgressiveParallelErrorDrainsBeforeClose(t *testing.T) {
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	var active atomic.Int32
	failure := errors.New("tile encoding failed")
	work := func(_ []byte, r []bitmapRect, _ bool, _ []int32) ([]byte, error) {
		active.Add(1)
		defer active.Add(-1)
		started <- struct{}{}
		if r[0].x == 0 {
			return nil, failure
		}
		<-release
		return []byte{1}, nil
	}
	var codecs []*parallelTestCodec
	makeCodec := func() (progressiveTileCodec, error) {
		c := &parallelTestCodec{encodeWork: work}
		codecs = append(codecs, c)
		return c, nil
	}
	first, _ := makeCodec()
	pool := newParallelProgressive(first, 6, 4, makeCodec)
	returned := make(chan error, 1)
	go func() {
		_, err := pool.encode(make([]byte, 512*64*4), 512, 64, parallelTestRects(), 1, true, progressiveBGRA)
		returned <- err
	}()
	for range 4 {
		<-started
	}
	select {
	case <-returned:
		t.Fatal("returned before remaining workers drained")
	default:
	}
	closed := make(chan struct{})
	go func() { pool.close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("close freed a busy encoder")
	default:
	}
	close(release)
	if err := <-returned; !errors.Is(err, failure) {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("close did not join workers")
	}
	if active.Load() != 0 {
		t.Fatal("workers still access input after close")
	}
	for _, c := range codecs {
		if !c.closed.Load() {
			t.Fatal("codec was not closed")
		}
	}
	pool.close()
	if _, err := pool.encode(nil, 1, 1, nil, 1, false, progressiveBGRA); err == nil {
		t.Fatal("closed pool accepted work")
	}
}

func TestProgressiveParallelSmallAndOverlappingBatches(t *testing.T) {
	first := &parallelTestCodec{encodeWork: func(_ []byte, _ []bitmapRect, _ bool, _ []int32) ([]byte, error) { return []byte{1}, nil }}
	pool := newParallelProgressive(first, 6, 4, func() (progressiveTileCodec, error) { t.Fatal("unexpected worker allocation"); return nil, nil })
	defer pool.close()
	pixels := make([]byte, 512*64*4)
	rects := parallelTestRects()
	for _, batch := range [][]bitmapRect{rects[:1], {rects[0], rects[1], rects[0], rects[1]}} {
		if _, err := pool.encode(pixels, 512, 64, batch, 1, true, progressiveBGRA); err != nil {
			t.Fatal(err)
		}
	}
	if first.serial.Load() != 2 {
		t.Fatal("small/overlapping batches did not use serial context")
	}
	if _, err := pool.encodePass(pixels, 512, 64, rects, 1, true, progressiveBGRA, 2, false, nil); err == nil {
		t.Fatal("invalid cache accepted")
	}
}

func TestProgressiveParallelLayeredLifecycle(t *testing.T) {
	modes := []string{"native-sse2", "native-avx2"}
	if runtime.GOARCH == "arm64" {
		modes = []string{"native"}
	}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			native, err := newNativeProgressive(mode, 6)
			if errors.Is(err, errProgressiveSIMDUnavailable) || errors.Is(err, errProgressiveNativeUnavailable) {
				t.Skip(err)
			}
			if err != nil {
				t.Fatal(err)
			}
			actual := &progressiveEncoder{native: native, required: true, first: true, quantization: 6, refineBits: 2}
			defer actual.close()
			want := &progressiveEncoder{first: true, quantization: 6, refineBits: 2}
			for _, format := range []progressivePixelFormat{progressiveBGRA, progressiveNV12} {
				for _, size := range [][2]int{{513, 257}, {129, 131}, {513, 257}} {
					w, h := size[0], size[1]
					pixels := progressivePixels(w, h, "gradient")
					if format == progressiveNV12 {
						pixels = progressiveNV12Pixels(w, h, "gradient")
					}
					rects := dirtyProgressiveRectsFormat(pixels, nil, w, h, format)
					compare := func(batch []bitmapRect, frame uint32) {
						a, err := actual.encodeWindow(pixels, w, h, batch, frame, format)
						var b [][]byte
						for i := 0; i < len(batch); {
							end := progressiveBatchEnd(batch, i)
							data, referenceErr := want.encodeFormat(pixels, w, h, batch[i:end], frame, format)
							if referenceErr != nil {
								t.Fatal(referenceErr)
							}
							b = append(b, data)
							i = end
						}
						if err != nil || !reflect.DeepEqual(a, b) || !reflect.DeepEqual(actual.coefficients, want.coefficients) || !reflect.DeepEqual(actual.pending, want.pending) {
							t.Fatalf("FIRST/cache mismatch: %v", err)
						}
					}
					compare(rects, 1)
					// Replace pending FIRST tiles in non-grid order, mixing a short
					// serial batch with a parallel batch across existing cache rows.
					pixels = progressivePixels(w, h, "noise")
					if format == progressiveNV12 {
						pixels = progressiveNV12Pixels(w, h, "noise")
					}
					compare(rects[:1], 2)
					batch := append([]bitmapRect(nil), rects[len(rects)-8:]...)
					for i, j := 0, len(batch)-1; i < j; i, j = i+1, j-1 {
						batch[i], batch[j] = batch[j], batch[i]
					}
					compare(batch, 3)
					for pending := actual.pendingRects(8); len(pending) != 0; pending = actual.pendingRects(8) {
						a, err := actual.upgrade(pending, 4)
						b, referenceErr := want.upgrade(pending, 4)
						if err != nil || referenceErr != nil || !bytes.Equal(a, b) || !reflect.DeepEqual(actual.coefficients, want.coefficients) {
							t.Fatalf("UPGRADE/cache mismatch: %v / %v", err, referenceErr)
						}
					}
					compare(rects[:8], 5)
					actual.discardRefinements()
					want.discardRefinements()
					if _, err := actual.upgrade(rects[:1], 6); err == nil {
						t.Fatal("refresh left stale upgrades eligible")
					}
					compare(rects[len(rects)-8:], 7)
				}
			}
		})
	}
}

func TestProgressiveParallelWindowOrderAndFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("failure=%v", fail), func(t *testing.T) {
			started := make(chan int, 4)
			release := [4]chan struct{}{}
			for i := range release {
				release[i] = make(chan struct{})
			}
			failure := errors.New("window encoding failed")
			var active atomic.Int32
			work := func(p []byte, r []bitmapRect, _ bool, cache []int32) ([]byte, error) {
				active.Add(1)
				defer active.Add(-1)
				group := r[0].x / (16 * 64)
				if r[0].x%(16*64) == 0 {
					started <- group
					<-release[group]
				}
				if fail && r[0].x == 0 {
					return nil, failure
				}
				data := make([]byte, 0, len(r))
				for _, rect := range r {
					index := rect.x / 64
					cache[index*progressiveTileCoefficients] = int32(p[0])
					data = append(data, byte(index))
				}
				return data, nil
			}
			var codecs []*parallelTestCodec
			create := func() (progressiveTileCodec, error) {
				c := &parallelTestCodec{encodeWork: work}
				codecs = append(codecs, c)
				return c, nil
			}
			first, _ := create()
			pool := newParallelProgressive(first, 6, 4, create)
			e := &progressiveEncoder{native: pool, first: true, required: true, quantization: 6, refineBits: 2}
			pixels := make([]byte, 4096*64*4)
			pixels[0] = 7
			rects := dirtyProgressiveRects(pixels, nil, 4096, 64)
			type result struct {
				packets [][]byte
				err     error
			}
			returned := make(chan result, 1)
			go func() {
				packets, err := e.encodeWindow(pixels, 4096, 64, rects, 23, progressiveBGRA)
				returned <- result{packets, err}
			}()
			for range 4 {
				<-started
			}
			// Later workers finish first, but no state or output may commit early.
			for i := 3; i > 0; i-- {
				close(release[i])
			}
			select {
			case <-returned:
				t.Fatal("window returned while first worker was busy")
			default:
			}
			if !e.first {
				t.Fatal("FIRST state committed before workers drained")
			}
			close(release[0])
			got := <-returned
			if active.Load() != 0 {
				t.Fatal("worker retained input after return")
			}
			if fail {
				if !errors.Is(got.err, failure) || got.packets != nil || !e.first || len(e.pendingRects(64)) != 0 {
					t.Fatal("failed window committed state", got.err)
				}
			} else {
				if got.err != nil || e.first || len(e.pendingRects(64)) != 64 {
					t.Fatal("window state mismatch", got.err)
				}
				for i, packet := range got.packets {
					payload := make([]byte, 8)
					for j := range payload {
						payload[j] = byte(i*8 + j)
					}
					want := progressiveLayeredWire(rects[i*8:(i+1)*8], 23, i == 0, 6, progressiveFirst50, false, payload, 8)
					if !bytes.Equal(packet, want) {
						t.Fatalf("packet %d lost FIRST/wire order", i)
					}
				}
				for i := 0; i < 64; i++ {
					if e.coefficients[i*progressiveTileCoefficients] != 7 {
						t.Fatal("wrong coefficient cache tile")
					}
				}
			}
			e.close()
			for _, c := range codecs {
				if !c.closed.Load() {
					t.Fatal("worker context not closed")
				}
			}
		})
	}
}

func TestProgressiveWindowPacketBoundaries(t *testing.T) {
	var rects []bitmapRect
	// Alternating four-tile and one-tile rectangles leave partially filled packets.
	for y := 0; y < 4096; y += 128 {
		rects = append(rects, bitmapRect{0, y, 128, 128}, bitmapRect{128, y, 64, 64}, bitmapRect{192, y, 128, 128})
	}
	next := 0
	for start := 0; start < len(rects); {
		end := progressiveWindowEnd(rects, start)
		for next < end {
			next = progressiveBatchEnd(rects, next)
		}
		if next != end {
			t.Fatal("window split a wire packet")
		}
		tiles := 0
		for _, r := range rects[start:end] {
			tiles += progressiveRectTiles(r)
		}
		if tiles > progressiveWindowTiles {
			t.Fatal("window exceeded tile limit")
		}
		start = end
	}
}

func TestProgressiveGraphicsAppend(t *testing.T) {
	data := []byte{1, 2, 3, 4, 5}
	want := gfxPDU(2, join(u16(1), u16(int(codecProgressive)), u32(1), []byte{0x20}, u32(uint32(len(data))), data))
	prefix := []byte{11, 12, 13}
	got := appendGraphicsProgressive(append([]byte(nil), prefix...), data)
	if !bytes.Equal(got, append(prefix, want...)) {
		t.Fatal("GFX packet changed")
	}
}

func TestProgressiveParallelWindowFallback(t *testing.T) {
	failure := errors.New("injected packet error")
	var active atomic.Int32
	work := func(_ []byte, r []bitmapRect, _ bool, cache []int32) ([]byte, error) {
		active.Add(1)
		defer active.Add(-1)
		for _, rect := range r {
			cache[(rect.x/64)*progressiveTileCoefficients] = 999
		}
		if r[0].x == 0 {
			return nil, failure
		}
		return []byte{1}, nil
	}
	var codecs []*parallelTestCodec
	create := func() (progressiveTileCodec, error) {
		c := &parallelTestCodec{encodeWork: work}
		codecs = append(codecs, c)
		return c, nil
	}
	first, _ := create()
	e := &progressiveEncoder{native: newParallelProgressive(first, 6, 4, create), first: true, quantization: 6, refineBits: 2}
	defer e.close()
	reference := &progressiveEncoder{first: true, quantization: 6, refineBits: 2}
	pixels := progressivePixels(4096, 64, "gradient")
	rects := dirtyProgressiveRects(pixels, nil, 4096, 64)
	actual, err := e.encodeWindow(pixels, 4096, 64, rects, 13, progressiveBGRA)
	want, referenceErr := reference.encodeWindow(pixels, 4096, 64, rects, 13, progressiveBGRA)
	if err != nil || referenceErr != nil || !reflect.DeepEqual(actual, want) || !reflect.DeepEqual(e.coefficients, reference.coefficients) || !reflect.DeepEqual(e.pending, reference.pending) {
		t.Fatal("failed native FIRST window was not fully recomputed", err, referenceErr)
	}
	if e.native != nil || active.Load() != 0 {
		t.Fatal("fallback retained native workers")
	}
	for _, c := range codecs {
		if !c.closed.Load() {
			t.Fatal("fallback did not close worker")
		}
	}
}

func TestProgressiveParallelWindowRetainsOutput(t *testing.T) {
	for _, mode := range []string{"native-sse2", "native-avx2", "native"} {
		if mode == "native" && runtime.GOARCH != "arm64" {
			continue
		}
		t.Run(mode, func(t *testing.T) {
			codec, err := newNativeProgressive(mode, 6)
			if errors.Is(err, errProgressiveSIMDUnavailable) || errors.Is(err, errProgressiveNativeUnavailable) {
				t.Skip(err)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer codec.close()
			serial, err := newNativeProgressive("native-scalar", 6)
			if err != nil {
				t.Fatal(err)
			}
			defer serial.close()
			const w, h = 2048, 128
			var rects []bitmapRect
			for x := 0; x < w; x += 128 {
				rects = append(rects, bitmapRect{x, 0, 128, 128})
			}
			pixels := progressivePixels(w, h, "noise")
			e := &progressiveEncoder{native: codec, first: true, required: true, quantization: 6}
			output, err := e.encodeWindow(pixels, w, h, rects, 19, progressiveBGRA)
			if err != nil {
				t.Fatal(err)
			}
			var saved [][]byte
			for i, start := 0, 0; start < len(rects); i++ {
				end := progressiveBatchEnd(rects, start)
				reference, err := serial.encode(pixels, w, h, rects[start:end], 19, i == 0, progressiveBGRA)
				if err != nil || !bytes.Equal(output[i], reference) {
					t.Fatal("coalesced window changed packet", i, err)
				}
				saved = append(saved, append([]byte(nil), output[i]...))
				start = end
			}
			for range 3 {
				if _, err := e.encodeWindow(progressivePixels(w, h, "solid"), w, h, rects, 20, progressiveBGRA); err != nil {
					t.Fatal(err)
				}
			}
			e.close()
			if !reflect.DeepEqual(output, saved) {
				t.Fatal("reuse/close changed retained output")
			}
		})
	}
}
