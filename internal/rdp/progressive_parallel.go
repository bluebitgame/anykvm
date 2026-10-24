package rdp

import (
	"errors"
	"sync"
)

type progressiveTileCodec interface {
	nativeProgressiveCodec
	nativeProgressiveLayered
	encodeTiles([]byte, int, int, []bitmapRect, progressivePixelFormat, int, bool, []int32) ([]byte, error)
}

type progressiveTileResult struct {
	data []byte
	err  error
}

type progressivePacket struct {
	rects []bitmapRect
	data  []byte
	err   error
}

type progressiveTileWork struct {
	packets   []progressivePacket
	frame     uint32
	first     bool
	pixels    []byte
	w, h      int
	rects     []bitmapRect
	format    progressivePixelFormat
	bits      int
	upgrade   bool
	cache     []int32
	result    *progressiveTileResult
	completed *sync.WaitGroup
}

// One batch stays bounded to eight tiles. Workers own their scratch/output
// buffers; only disjoint tile ranges of the caller's coefficient cache are shared.
type parallelProgressive struct {
	mu           sync.Mutex
	codecs       []progressiveTileCodec
	create       func() (progressiveTileCodec, error)
	quantization int
	label        string
	limit        int
	jobs         chan progressiveTileWork
	running      int
	stopped      sync.WaitGroup
	closed       bool
}

func newParallelProgressive(first progressiveTileCodec, quantization, workers int, create func() (progressiveTileCodec, error)) *parallelProgressive {
	return &parallelProgressive{
		codecs: []progressiveTileCodec{first}, create: create,
		quantization: quantization, label: first.name(), limit: max(1, workers), jobs: make(chan progressiveTileWork, max(1, workers)),
	}
}

func (p *parallelProgressive) name() string { return p.label }

func (p *parallelProgressive) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	close(p.jobs)
	p.stopped.Wait()
	for _, codec := range p.codecs {
		codec.close()
	}
}

func (p *parallelProgressive) startWorkers(count int) error {
	for len(p.codecs) < count {
		codec, err := p.create()
		if err != nil {
			return err
		}
		p.codecs = append(p.codecs, codec)
	}
	for p.running < count {
		codec := p.codecs[p.running]
		p.running++
		p.stopped.Add(1)
		go func() {
			defer p.stopped.Done()
			for work := range p.jobs {
				if group, ok := codec.(interface{ encodePackets(progressiveTileWork) }); ok && len(work.packets) != 0 {
					group.encodePackets(work)
				} else if len(work.packets) != 0 {
					for i := range work.packets {
						packet := &work.packets[i]
						packet.data, packet.err = codec.encodePass(work.pixels, work.w, work.h, packet.rects,
							work.frame, work.first && i == 0, work.format, work.bits, work.upgrade, work.cache)
					}
				} else {
					work.result.data, work.result.err = codec.encodeTiles(work.pixels, work.w, work.h, work.rects,
						work.format, work.bits, work.upgrade, work.cache)
				}
				work.completed.Done()
			}
		}()
	}
	return nil
}

func (p *parallelProgressive) encode(pixels []byte, w, h int, rects []bitmapRect, frame uint32, first bool, format progressivePixelFormat) ([]byte, error) {
	return p.encodePass(pixels, w, h, rects, frame, first, format, 0, false, nil)
}

func (p *parallelProgressive) encodePass(pixels []byte, w, h int, rects []bitmapRect, frame uint32, first bool, format progressivePixelFormat, bits int, upgrade bool, cache []int32) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errors.New("Progressive encoder closed")
	}
	if w < 1 || h < 1 || w > 8192 || h > 8192 || len(rects) < 1 || len(rects) > progressiveBatchRects ||
		(format != progressiveBGRA && format != progressiveNV12) || (!upgrade && len(pixels) != progressivePixelBytes(w, h, format)) {
		return nil, errors.New("invalid Progressive frame or batch")
	}
	if !validProgressivePass(bits, upgrade) ||
		(bits != 0 && len(cache) != ((w+63)/64)*((h+63)/64)*progressiveTileCoefficients) {
		return nil, errors.New("invalid Progressive pass or coefficient cache")
	}
	var tiles [progressiveBatchTiles]bitmapRect
	count, overlap := 0, false
	for _, r := range rects {
		if r.x < 0 || r.y < 0 || r.x >= w || r.y >= h || r.x%64 != 0 || r.y%64 != 0 ||
			r.w < 1 || r.h < 1 || r.w > 128 || r.h > 128 || r.x+r.w > w || r.y+r.h > h {
			return nil, errors.New("invalid Progressive rectangle")
		}
		for y := r.y; y < r.y+r.h; y += 64 {
			for x := r.x; x < r.x+r.w; x += 64 {
				if count == len(tiles) {
					return nil, errors.New("Progressive batch exceeds eight tiles")
				}
				for _, prior := range tiles[:count] {
					if prior.x == x && prior.y == y {
						overlap = true
					}
				}
				tiles[count] = bitmapRect{x, y, min(64, r.x+r.w-x), min(64, r.y+r.h-y)}
				count++
			}
		}
	}
	// Small batches avoid dispatch overhead. Overlapping rectangles retain the
	// serial wire order and cannot race while replacing a FIRST cache entry.
	workers := min(p.limit, count/2)
	if workers < 2 || overlap {
		return p.codecs[0].encodePass(pixels, w, h, rects, frame, first, format, bits, upgrade, cache)
	}
	if err := p.startWorkers(workers); err != nil {
		return nil, err
	}
	results := make([]progressiveTileResult, workers)
	var completed sync.WaitGroup
	completed.Add(workers)
	for i := range results {
		p.jobs <- progressiveTileWork{pixels: pixels, w: w, h: h, rects: tiles[count*i/workers : count*(i+1)/workers],
			format: format, bits: bits, upgrade: upgrade, cache: cache, result: &results[i], completed: &completed}
	}
	// Drain every job before returning, including on failure. The caller may
	// then replace/release input and caches, retry in Go, resize, or close safely.
	completed.Wait()
	size := 0
	for _, result := range results {
		if result.err != nil {
			return nil, result.err
		}
		size += len(result.data)
	}
	if size > 256<<10 {
		return nil, errors.New("Progressive batch exceeds 256 KiB")
	}
	payload := make([]byte, 0, size)
	for _, result := range results {
		payload = append(payload, result.data...)
	}
	var wire []byte
	if bits != 0 {
		wire = progressiveLayeredWire(rects, frame, first, p.quantization, bits, upgrade, payload, count)
	} else {
		wire = progressiveSimpleWire(rects, frame, first, p.quantization, payload, count)
	}
	if len(wire) > 256<<10 {
		return nil, errors.New("Progressive batch exceeds 256 KiB")
	}
	return wire, nil
}

// Foreground captures amortize dispatch across several packets. Every packet
// still has at most eight tiles; background UPGRADE keeps the smaller encodePass
// path so it can yield to a new capture after each packet.
func (p *parallelProgressive) encodeBatches(pixels []byte, w, h int, rects []bitmapRect, frame uint32, first bool, format progressivePixelFormat, bits int, cache []int32) ([][]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errors.New("Progressive encoder closed")
	}
	if w < 1 || h < 1 || w > 8192 || h > 8192 || len(rects) == 0 || len(rects) > progressiveWindowTiles ||
		(format != progressiveBGRA && format != progressiveNV12) || len(pixels) != progressivePixelBytes(w, h, format) ||
		!validProgressivePass(bits, false) || (bits != 0 && len(cache) != ((w+63)/64)*((h+63)/64)*progressiveTileCoefficients) {
		return nil, errors.New("invalid Progressive window")
	}
	// Duplicate FIRST tiles must retain serial cache replacement semantics.
	var seen [256]uint64 // Maximum 8192 x 8192 surface, indexed by 64 x 64 tile.
	count, overlap := 0, false
	for _, r := range rects {
		if r.x < 0 || r.y < 0 || r.x >= w || r.y >= h || r.x%64 != 0 || r.y%64 != 0 ||
			r.w < 1 || r.h < 1 || r.w > 128 || r.h > 128 || r.x+r.w > w || r.y+r.h > h {
			return nil, errors.New("invalid Progressive rectangle")
		}
		for y := r.y; y < r.y+r.h; y += 64 {
			for x := r.x; x < r.x+r.w; x += 64 {
				index := progressiveTileIndex(w, x, y)
				mask := uint64(1) << (index % 64)
				overlap = overlap || seen[index/64]&mask != 0
				seen[index/64] |= mask
				count++
			}
		}
	}
	if count > progressiveWindowTiles {
		return nil, errors.New("Progressive window exceeds tile limit")
	}
	packets := make([]progressivePacket, 0, (count+progressiveBatchTiles-1)/progressiveBatchTiles)
	for i := 0; i < len(rects); {
		end := progressiveBatchEnd(rects, i)
		packets = append(packets, progressivePacket{rects: rects[i:end]})
		i = end
	}
	workers := min(p.limit, len(packets))
	if workers < 2 || overlap {
		for i := range packets {
			packet := &packets[i]
			packet.data, packet.err = p.codecs[0].encodePass(pixels, w, h, packet.rects, frame, first && i == 0, format, bits, false, cache)
		}
	} else {
		if err := p.startWorkers(workers); err != nil {
			return nil, err
		}
		var completed sync.WaitGroup
		completed.Add(workers)
		for i := 0; i < workers; i++ {
			start, end := len(packets)*i/workers, len(packets)*(i+1)/workers
			p.jobs <- progressiveTileWork{pixels: pixels, w: w, h: h, format: format, bits: bits, cache: cache,
				packets: packets[start:end], frame: frame, first: first && start == 0, completed: &completed}
		}
		// No error can release the frame/cache while another worker still uses it.
		completed.Wait()
	}
	output := make([][]byte, len(packets))
	for i, packet := range packets {
		if packet.err != nil {
			return nil, packet.err
		}
		output[i] = packet.data
	}
	return output, nil
}
