package jpegcodec

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"os"
	"testing"
)

func computeDecodeFixture(t *testing.T, restart bool) ([]byte, []int32) {
	t.Helper()
	const w, h = 35, 19
	pixels := make([]byte, w*h*4)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := (y*w + x) * 4
			pixels[i] = byte(x * 5)
			pixels[i+1] = byte(y * 11)
			pixels[i+2] = byte(220 - x*3)
			pixels[i+3] = 255
		}
	}
	coefficients := referenceCoefficients(pixels, w, h, 0, w, h, 80)
	data, err := encodeQuantized420Restart(coefficients, w, h, 80, restart)
	if err != nil {
		t.Fatal(err)
	}
	return data, coefficients
}

// Independent scalar entropy oracle checks parser table remapping, restart/DC
// boundaries and zigzag order against the known encoded coefficient fixture.
func entropyOracle(j *computeJPEG) ([]int32, error) {
	out := make([]int32, int(j.metadata[6]*j.metadata[7])*j.blocks*64)
	for s := 0; s < j.segments; s++ {
		segment := j.metadata[decodeSegmentBase+s*4:]
		pos, end := int(segment[0])*8, int(segment[0]+segment[1])*8
		read := func(n int) (int, error) {
			v := 0
			if pos+n > end {
				return 0, errors.New("truncated entropy")
			}
			for k := 0; k < n; k++ {
				v = v<<1 | int(j.entropy[pos/8]>>(7-pos%8)&1)
				pos++
			}
			return v, nil
		}
		symbol := func(table int) (int, error) {
			base := decodeHuffmanBase + table*decodeHuffmanWords
			code := 0
			for n := 0; n < 16; n++ {
				bit, err := read(1)
				if err != nil {
					return 0, err
				}
				code = code*2 + bit
				lo, hi := int(j.metadata[base+n]), int(j.metadata[base+16+n])
				if code >= lo && code < hi {
					return int(j.metadata[base+48+int(j.metadata[base+32+n])+code-lo]), nil
				}
			}
			return 0, errors.New("invalid Huffman code")
		}
		extend := func(n int) (int32, error) {
			v, err := read(n)
			if n > 0 && v < 1<<(n-1) {
				v -= 1<<n - 1
			}
			return int32(v), err
		}
		predictors := [3]int32{}
		for m := int(segment[2]); m < int(segment[2]+segment[3]); m++ {
			for c := 0; c < int(j.metadata[14]); c++ {
				cp := j.metadata[decodeComponentBase+c*8:]
				blocks := int(cp[0] * cp[1])
				for b := 0; b < blocks; b++ {
					base := (m*j.blocks + int(cp[2]) + b) * 64
					n, err := symbol(c * 2)
					if err != nil {
						return nil, err
					}
					dc, err := extend(n)
					if err != nil {
						return nil, err
					}
					predictors[c] += dc
					out[base] = predictors[c]
					for k := 1; k < 64; {
						ac, err := symbol(c*2 + 1)
						if err != nil {
							return nil, err
						}
						if ac == 0 {
							break
						}
						if ac == 240 {
							k += 16
							continue
						}
						k += ac >> 4
						if k >= 64 {
							return nil, errors.New("AC overflow")
						}
						v, err := extend(ac & 15)
						if err != nil {
							return nil, err
						}
						out[base+int(jpegZigzag[k])] = v
						k++
					}
				}
			}
		}
		remaining := end - pos
		if remaining > 7 {
			return nil, errors.New("trailing entropy")
		}
		padding, err := read(remaining)
		if err != nil || padding != (1<<remaining)-1 {
			return nil, errors.New("invalid padding")
		}
	}
	return out, nil
}

func TestComputeJPEGParserRestartAndTables(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprint(restart), func(t *testing.T) {
			data, want := computeDecodeFixture(t, restart)
			j, err := parseComputeJPEG(data, 64, 48)
			if err != nil {
				t.Fatal(err)
			}
			if j.width != 35 || j.height != 19 || j.blocks != 6 || j.segments != map[bool]int{false: 1, true: 6}[restart] {
				t.Fatal(j.width, j.height, j.blocks, j.segments)
			}
			got, err := entropyOracle(j)
			if err != nil {
				t.Fatal(err)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("coefficient %d = %d, want %d", i, got[i], want[i])
				}
			}
			if j.metadata[10] != 64 || j.metadata[11] != 34 || j.metadata[13] != 6 {
				t.Fatalf("fit geometry: %v", j.metadata[:16])
			}
		})
	}
}

func TestComputeJPEGParserRejectsUnsupportedAndTruncation(t *testing.T) {
	data, _ := computeDecodeFixture(t, true)
	for n := 0; n < len(data); n++ {
		if _, err := parseComputeJPEG(data[:n], 32, 24); err == nil {
			t.Fatalf("accepted truncation at %d", n)
		}
	}
	for _, marker := range []byte{0xc1, 0xc2, 0xc3} {
		bad := bytes.Replace(data, []byte{255, 0xc0}, []byte{255, marker}, 1)
		if _, err := parseComputeJPEG(bad, 32, 24); !errors.Is(err, ErrComputeJPEG) {
			t.Fatal("accepted nonbaseline", marker)
		}
	}
	bad := bytes.Replace(data, []byte{255, 0xd0}, []byte{255, 0xd3}, 1)
	if _, err := parseComputeJPEG(bad, 32, 24); err == nil {
		t.Fatal("accepted wrong restart sequence")
	}
	if _, err := parseComputeJPEG(data, 31, 24); err == nil {
		t.Fatal("accepted odd NV12 dimensions")
	}
	// A zero quantizer and an oversubscribed Huffman tree must fail on the host.
	for _, marker := range []byte{0xdb, 0xc4} {
		bad := append([]byte(nil), data...)
		i := bytes.Index(bad, []byte{255, marker})
		if marker == 0xdb {
			bad[i+5] = 0
		} else {
			bad[i+5] = 255
		}
		if _, err := parseComputeJPEG(bad, 32, 24); err == nil {
			t.Fatal("accepted malformed table", marker)
		}
	}
}

func TestComputeJPEGParserGrayscale(t *testing.T) {
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewGray(image.Rect(0, 0, 19, 17)), nil); err != nil {
		t.Fatal(err)
	}
	j, err := parseComputeJPEG(b.Bytes(), 20, 18)
	if err != nil {
		t.Fatal(err)
	}
	if j.blocks != 1 || j.metadata[14] != 1 {
		t.Fatal(j.metadata[:16])
	}
	if _, err := entropyOracle(j); err != nil {
		t.Fatal(err)
	}
}

func FuzzComputeJPEGParser(f *testing.F) {
	var b bytes.Buffer
	jpeg.Encode(&b, image.NewGray(image.Rect(0, 0, 8, 8)), nil)
	f.Add(b.Bytes())
	f.Add([]byte{255, 216, 255, 217})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			t.Skip()
		}
		j, err := parseComputeJPEG(data, 64, 48)
		if err == nil {
			if len(j.entropy)%4 != 0 || len(j.metadata) != decodeSegmentBase+j.segments*4 || j.metadata[9] != uint32(j.segments) {
				t.Fatal("invalid compute metadata")
			}
		}
	})
}

// Run on a real Windows/Linux GPU with ANYKVM_TEST_GPU_JPEG_DECODE=1.
// The Go JPEG decoder is an independent pixel/color oracle, not the shader code.
func TestGPUJPEGDecodeNV12(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_GPU_JPEG_DECODE") == "" {
		t.Skip("set ANYKVM_TEST_GPU_JPEG_DECODE=1 on Windows/Linux")
	}
	d, err := NewNV12Decoder()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, restart := range []bool{false, true} {
		data, _ := computeDecodeFixture(t, restart)
		for _, size := range [][2]int{{36, 20}, {64, 48}, {2, 2}, {36, 20}} {
			got, err := d.Decode(data, size[0], size[1])
			if err != nil {
				t.Fatal(err)
			}
			img, err := jpeg.Decode(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			want := oracleNV12(img, size[0], size[1])
			if len(got) != len(want) {
				t.Fatal("wrong NV12 size")
			}
			for i := range want {
				delta := int(got[i]) - int(want[i])
				if delta < -4 || delta > 4 {
					t.Fatalf("restart=%v size=%v byte=%d got=%d want=%d", restart, size, i, got[i], want[i])
				}
			}
		}
	}
	for _, sampling := range [][2]int{{1, 1}, {2, 1}, {1, 2}, {2, 2}} {
		data, _ := sampledComputeFixture(t, sampling[0], sampling[1])
		got, err := d.Decode(data, 20, 18)
		if err != nil {
			t.Fatal(err)
		}
		img, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		want := oracleNV12(img, 20, 18)
		for i := range want {
			delta := int(got[i]) - int(want[i])
			if delta < -4 || delta > 4 {
				t.Fatalf("sampling=%v byte=%d got=%d want=%d", sampling, i, got[i], want[i])
			}
		}
	}
	var gray bytes.Buffer
	jpeg.Encode(&gray, image.NewGray(image.Rect(0, 0, 19, 17)), nil)
	gotGray, err := d.Decode(gray.Bytes(), 20, 18)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range gotGray[20*18:] {
		if v != 128 {
			t.Fatal("grayscale chroma", v)
		}
	}
	// Header parsing succeeds but a truncated entropy segment must be rejected by
	// the GPU, not returned as uninitialized pixels. Reuse after the failure.
	data, _ := computeDecodeFixture(t, false)
	j, err := parseComputeJPEG(data, 36, 20)
	if err != nil {
		t.Fatal(err)
	}
	j.metadata[decodeSegmentBase+1] = 1
	if _, err := d.backend.decode(j); err == nil {
		t.Fatal("accepted invalid GPU entropy")
	}
	if _, err := d.Decode(data, 36, 20); err != nil {
		t.Fatal("decoder failed to recover", err)
	}
	d.Close()
	d.Close()
	if _, err := d.Decode(data, 36, 20); err == nil {
		t.Fatal("accepted closed decoder")
	}
}

func oracleNV12(img image.Image, w, h int) []byte {
	sw, sh := img.Bounds().Dx(), img.Bounds().Dy()
	dw, dh := w, h
	if sw*dh > sh*dw {
		dh = max(2, (sh*dw/sw)&^1)
	} else {
		dw = max(2, (sw*dh/sh)&^1)
	}
	ox, oy := ((w-dw)/2)&^1, ((h-dh)/2)&^1
	sample := func(x, y int) [3]float64 {
		if x < ox || y < oy || x >= ox+dw || y >= oy+dh {
			return [3]float64{16, 128, 128}
		}
		c := color.NRGBAModel.Convert(img.At((x-ox)*sw/dw, (y-oy)*sh/dh)).(color.NRGBA)
		r, g, b := float64(c.R), float64(c.G), float64(c.B)
		return [3]float64{16 + (.2126*r+.7152*g+.0722*b)*219/255, 128 + (-.114572*r-.385428*g+.5*b)*224/255, 128 + (.5*r-.454153*g-.045847*b)*224/255}
	}
	out := make([]byte, w*h*3/2)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			out[y*w+x] = byte(math.Round(sample(x, y)[0]))
		}
	}
	for y := 0; y < h; y += 2 {
		for x := 0; x < w; x += 2 {
			u, v := 0., 0.
			for j := 0; j < 2; j++ {
				for i := 0; i < 2; i++ {
					s := sample(x+i, y+j)
					u += s[1] / 4
					v += s[2] / 4
				}
			}
			p := w*h + y/2*w + x
			out[p] = byte(math.Round(u))
			out[p+1] = byte(math.Round(v))
		}
	}
	return out
}

// Guard the host/shader fixed metadata ABI independently of the C bindings.
func TestComputeJPEGMetadataABI(t *testing.T) {
	if decodeSegmentBase != 2056 {
		t.Fatal(decodeSegmentBase)
	}
	for _, entry := range []string{"main", "inverseDCT", "packNV12"} {
		suffix := ""
		if entry != "main" {
			suffix = "_" + entry
		}
		data, err := os.ReadFile("shaders/jpeg_decode" + suffix + ".spv")
		if os.IsNotExist(err) {
			t.Skip("prepare shaders")
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(data) < 20 || len(data)%4 != 0 || binary.LittleEndian.Uint32(data) != 0x07230203 {
			t.Fatal("invalid SPIR-V")
		}
	}
}

func sampledComputeFixture(t *testing.T, h, v int) ([]byte, []int32) {
	t.Helper()
	const w, height = 19, 17
	header := jpegHeader420(w, height, 80, false)
	sof := bytes.Index(header, []byte{255, 0xc0})
	header[sof+11] = byte(h<<4 | v)
	blocks := h*v + 2
	count := ((w + 8*h - 1) / (8 * h)) * ((height + 8*v - 1) / (8 * v))
	coeff := make([]int32, count*blocks*64)
	bits := jpegBits{data: header}
	previous := [3]int32{}
	for m := 0; m < count; m++ {
		for block := 0; block < blocks; block++ {
			c, table := 0, 0
			if block >= h*v {
				c = block - h*v + 1
				table = 2
			}
			base := (m*blocks + block) * 64
			coeff[base] = int32((m*17+block*11)%80 - 40)
			coeff[base+1] = int32(block%7 - 3)
			value, size := jpegMagnitude(coeff[base] - previous[c])
			previous[c] = coeff[base]
			bits.symbol(table, byte(size))
			bits.put(value, size)
			if coeff[base+1] != 0 {
				value, size = jpegMagnitude(coeff[base+1])
				bits.symbol(table+1, byte(size))
				bits.put(value, size)
			}
			bits.symbol(table+1, 0)
		}
	}
	if bits.count > 0 {
		bits.put(0x7f, 8-bits.count)
	}
	bits.data = append(bits.data, 255, 217)
	if _, err := jpeg.Decode(bytes.NewReader(bits.data)); err != nil {
		t.Fatal("invalid independent fixture", err)
	}
	return bits.data, coeff
}

func TestComputeJPEGSubsampling(t *testing.T) {
	for _, sampling := range [][2]int{{1, 1}, {2, 1}, {1, 2}, {2, 2}} {
		t.Run(fmt.Sprint(sampling), func(t *testing.T) {
			data, want := sampledComputeFixture(t, sampling[0], sampling[1])
			j, err := parseComputeJPEG(data, 20, 18)
			if err != nil {
				t.Fatal(err)
			}
			got, err := entropyOracle(j)
			if err != nil {
				t.Fatal(err)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("coefficient %d=%d want %d", i, got[i], want[i])
				}
			}
		})
	}
}

func TestVideoJPEGDecoderFallbackAndDimensions(t *testing.T) {
	var d VideoDecoder
	defer d.Close()
	data, _ := computeDecodeFixture(t, false)
	// Skip compute initialization deterministically; fallback retains source size.
	d.tried = true
	pixels, err := d.Decode(data, 35, 19, 64, 48)
	if err != nil {
		t.Fatal(err)
	}
	if pixels.Format != "bgra" || pixels.Width != 35 || pixels.Height != 19 || len(pixels.Data) != 35*19*4 {
		t.Fatal(pixels.Width, pixels.Height, pixels.Format)
	}
	if _, err := d.Decode(data, 34, 19, 64, 48); err == nil {
		t.Fatal("accepted source dimension mismatch")
	}
	if _, err := d.Decode([]byte("invalid"), 35, 19, 64, 48); err == nil {
		t.Fatal("accepted invalid JPEG")
	}
	d.Close()
	d.Close()
}
