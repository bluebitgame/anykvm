package avmedia

import (
	"anykvm/internal/framerate"
	"bytes"
	"image"
	"image/jpeg"
	"io"
	"os"
	"testing"
)

func TestLibraryJPEGEncoder(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_AVMEDIA") == "" {
		t.Skip("set ANYKVM_TEST_AVMEDIA and ANYKVM_FFMPEG_LIBDIR")
	}
	for _, size := range [][2]int{{1, 1}, {37, 25}, {64, 48}} {
		for _, quality := range []int{1, 80, 100} {
			w, h := size[0], size[1]
			encoder, err := NewJPEGEncoder(w, h, quality)
			if err != nil {
				t.Fatal(err)
			}
			pixels := make([]byte, w*h*4)
			for i := range pixels {
				pixels[i] = byte(i * 13)
			}
			packet, err := encoder.EncodeOwned(pixels, w, h, "bgra", false)
			encoder.Close()
			if err != nil || packet == nil {
				t.Fatalf("JPEG %dx%d quality %d: %v", w, h, quality, err)
			}
			// Output must survive closing the encoder and preserve odd dimensions.
			config, err := jpeg.DecodeConfig(bytes.NewReader(packet.Bytes()))
			packet.Release()
			if err != nil || config.Width != w || config.Height != h {
				t.Fatalf("JPEG dimensions %dx%d: %+v, %v", w, h, config, err)
			}
		}
	}
}

func TestLibraryJPEGFullRange(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_AVMEDIA") == "" {
		t.Skip("set ANYKVM_TEST_AVMEDIA and ANYKVM_FFMPEG_LIBDIR")
	}
	decoder, err := NewDecoder("mjpeg")
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	// Full-range gray must retain its intensity, including values that limited
	// range would clamp to black/white. Reuse the decoder across frames.
	for _, gray := range []byte{0, 16, 128, 235, 255} {
		picture := image.NewRGBA(image.Rect(0, 0, 32, 32))
		for i := 0; i < len(picture.Pix); i += 4 {
			picture.Pix[i], picture.Pix[i+1], picture.Pix[i+2], picture.Pix[i+3] = gray, gray, gray, 255
		}
		var packet bytes.Buffer
		if err := jpeg.Encode(&packet, picture, &jpeg.Options{Quality: 100}); err != nil {
			t.Fatal(err)
		}
		frame, err := decoder.Decode(packet.Bytes())
		if err != nil || frame == nil {
			t.Fatalf("gray %d: decode: %v", gray, err)
		}
		if frame.Width != 32 || frame.Height != 32 || len(frame.Data) != 32*32*4 {
			frame.Release()
			t.Fatalf("gray %d: unexpected dimensions", gray)
		}
		for i := 0; i < len(frame.Data); i += 4 {
			for channel := 0; channel < 3; channel++ {
				delta := int(frame.Data[i+channel]) - int(gray)
				if delta < -2 || delta > 2 {
					got := frame.Data[i+channel]
					frame.Release()
					t.Fatalf("gray %d: got %d at pixel %d", gray, got, i/4)
				}
			}
		}
		frame.Release()
	}
}

// All test inputs are generated in memory. No device or hardware codec is used.
func TestLibrarySoftwareRoundTrip(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_AVMEDIA") == "" {
		t.Skip("set ANYKVM_TEST_AVMEDIA and ANYKVM_FFMPEG_LIBDIR")
	}
	if err := Available(); err != nil {
		t.Fatal(err)
	}
	encoder, err := NewEncoder(VideoOptions{Width: 64, Height: 48, FPS: 30, Bitrate: 1000, Codec: "mjpeg"})
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	decoder, err := NewDecoder("mjpeg")
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	var previous []byte
	for n := 0; n < 3; n++ {
		pixels := make([]byte, 64*48*4)
		for i := 0; i < len(pixels); i += 4 {
			pixels[i] = byte(30 + n*70)
			pixels[i+1] = 100
			pixels[i+2] = 200
			pixels[i+3] = 255
		}
		packet, err := encoder.Encode(pixels, 64, 48, "bgra", false)
		if err != nil {
			t.Fatal(err)
		}
		if len(packet) < 4 || packet[0] != 255 || packet[1] != 216 {
			t.Fatal("not a JPEG packet")
		}
		frame, err := decoder.Decode(packet)
		if err != nil {
			t.Fatal(err)
		}
		if frame == nil || frame.Width != 64 || frame.Height != 48 || len(frame.Data) != 64*48*4 {
			t.Fatal("invalid decoded frame")
		}
		if n > 0 && bytes.Equal(frame.Data, previous) {
			t.Fatal("frozen output")
		}
		previous = frame.Data
	}
	if _, err := encoder.Encode([]byte{0}, 64, 48, "bgra", false); err == nil {
		t.Fatal("accepted truncated pixels")
	}
	encoder.Close()
	if _, err := encoder.Encode(make([]byte, 64*48*4), 64, 48, "bgra", false); err == nil {
		t.Fatal("encode after close")
	}
}
func TestLibrarySyntheticCapture(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_AVMEDIA") == "" {
		t.Skip("set ANYKVM_TEST_AVMEDIA")
	}
	source, err := OpenSource("lavfi", "testsrc2=size=64x48:rate=30", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	for i := 0; i < 3; i++ {
		f, err := source.Read()
		if err != nil || f == nil || f.Width != 64 || f.Height != 48 {
			t.Fatalf("frame=%v err=%v", f, err)
		}
	}
	source.Close()
	if _, err := source.Read(); err == nil {
		t.Fatal("read after close")
	}
}

func TestLibraryOpusPackets(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_AVMEDIA") == "" {
		t.Skip("set ANYKVM_TEST_AVMEDIA")
	}
	for _, duration := range []int{5, 10} {
		encoder, err := NewAudioEncoder(duration, 160)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(encoder.Close)
		pcm := make([]byte, duration*48*4) // 48 kHz, stereo, signed 16-bit PCM.
		for i := 0; i < 3; i++ {
			packet, err := encoder.Encode(pcm, 0, 0, "", false)
			if err != nil || len(packet) == 0 {
				t.Fatalf("Opus %d ms: packet size=%d err=%v", duration, len(packet), err)
			}
			config := packet[0] >> 3
			if config < 16 || packet[0]&3 != 0 || packet[0]&4 == 0 || 120<<(config&3) != duration*48 {
				t.Fatalf("unexpected Opus %d ms TOC: %02x", duration, packet[0])
			}
		}
		if _, err := encoder.Encode(pcm[:len(pcm)-4], 0, 0, "", false); err == nil {
			t.Fatal("accepted truncated PCM")
		}
		encoder.Close()
	}
}

func TestLibraryCompressedPassthrough(t *testing.T) {
	for _, codec := range []string{"h264", "hevc"} {
		t.Run(codec, func(t *testing.T) { testLibraryCompressedPassthrough(t, codec) })
	}
}
func testLibraryCompressedPassthrough(t *testing.T, codec string) {

	if os.Getenv("ANYKVM_TEST_AVMEDIA") == "" {
		t.Skip("set ANYKVM_TEST_AVMEDIA and ANYKVM_FFMPEG_LIBDIR")
	}
	input := "../testdata/passthrough.mp4"
	if codec == "hevc" {
		input = "../testdata/passthrough_hevc.mp4"
	}
	source, err := OpenVideoSource("mov", input, nil, codec)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if source.FrameRate() != framerate.New(30, 1) {
		t.Fatalf("source rate: %v", source.FrameRate())
	}
	var got []byte
	count := 0
	for {
		frame, err := source.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if frame == nil {
			continue
		}
		if frame.Codec != codec || frame.Width != 64 || frame.Height != 48 {
			t.Fatalf("lost compressed source: %+v", frame)
		}
		got = append(got, frame.Data...)
		count++
	}
	want, err := os.ReadFile("../testdata/passthrough." + codec)
	if err != nil {
		t.Fatal(err)
	}
	if count != 4 || !bytes.Equal(got, want) {
		t.Fatalf("passthrough altered access units: frames=%d bytes=%d want=%d", count, len(got), len(want))
	}
	// A consumer requiring pixels still takes the decoder path.
	decoded, err := OpenSource("mov", input, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer decoded.Close()
	frame, err := decoded.Read()
	if err != nil || frame == nil || frame.Codec != "" || len(frame.Data) != 64*48*4 {
		t.Fatalf("decoded source: %v %v", frame, err)
	}
}

func TestLibraryFractionalRate(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_AVMEDIA") == "" {
		t.Skip("set ANYKVM_TEST_AVMEDIA")
	}
	rate := framerate.New(30000, 1001)
	source, err := OpenSource("lavfi", "testsrc2=size=64x48:rate=30000/1001", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if source.FrameRate() != rate {
		t.Fatalf("source rate: %v", source.FrameRate())
	}
	encoder, err := NewEncoder(VideoOptions{Width: 64, Height: 48, Rate: rate, Bitrate: 1000, Codec: "libx264"})
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	var encoded []byte
	for i := 0; i < 4; i++ {
		frame, err := source.Read()
		if err != nil {
			t.Fatal(err)
		}
		packet, err := encoder.Encode(frame.Data, 64, 48, "bgra", i == 0)
		if err != nil {
			t.Fatal(err)
		}
		encoded = append(encoded, packet...)
	}
	if got := baselineH264Rate(t, encoded); got != rate {
		t.Fatalf("encoded bitstream rate: %v, want %v", got, rate)
	}
}

// Read the VUI timing fields from the baseline SPS produced by this test.
func baselineH264Rate(t *testing.T, encoded []byte) framerate.Rate {
	t.Helper()
	var rbsp []byte
	for _, nal := range bytes.Split(encoded, []byte{0, 0, 1}) {
		if len(nal) < 2 || nal[0]&31 != 7 {
			continue
		}
		payload := nal[1:]
		for i, b := range payload {
			if i >= 2 && b == 3 && payload[i-1] == 0 && payload[i-2] == 0 {
				continue
			}
			rbsp = append(rbsp, b)
		}
		break
	}
	pos := 0
	bits := func(n int) uint32 {
		if pos+n > len(rbsp)*8 {
			t.Fatal("truncated SPS")
		}
		var v uint32
		for i := 0; i < n; i++ {
			v = v<<1 | uint32((rbsp[pos/8]>>uint(7-pos%8))&1)
			pos++
		}
		return v
	}
	ue := func() uint32 {
		zeros := 0
		for bits(1) == 0 {
			zeros++
			if zeros > 31 {
				t.Fatal("invalid Exp-Golomb value")
			}
		}
		return (uint32(1) << uint(zeros)) - 1 + bits(zeros)
	}
	if bits(8) != 66 {
		t.Fatal("expected baseline profile")
	}
	if bits(8)&0x40 == 0 {
		t.Fatal("missing constrained Baseline flag")
	}
	bits(8)       // Level.
	ue()          // SPS ID.
	ue()          // Frame number width.
	switch ue() { // Picture order count type.
	case 0:
		ue()
	case 1:
		bits(1)
		ue()
		ue()
		count := ue()
		for i := uint32(0); i < count; i++ {
			ue()
		}
	}
	ue()
	bits(1) // Reference frames and gaps flag.
	ue()
	ue() // Dimensions.
	if bits(1) == 0 {
		bits(1)
	} // Interlaced coding.
	bits(1)
	if bits(1) != 0 {
		ue()
		ue()
		ue()
		ue()
	} // Cropping.
	if bits(1) == 0 {
		t.Fatal("missing VUI")
	}
	if bits(1) != 0 && bits(8) == 255 {
		bits(16)
		bits(16)
	} // Aspect ratio.
	if bits(1) != 0 {
		bits(1)
	} // Overscan.
	if bits(1) != 0 { // Video signal type.
		bits(3)
		bits(1)
		if bits(1) != 0 {
			bits(24)
		}
	}
	if bits(1) != 0 {
		ue()
		ue()
	} // Chroma location.
	if bits(1) == 0 {
		t.Fatal("missing timing info")
	}
	units, scale := bits(32), bits(32)
	return framerate.New(int(scale), int(2*units))
}
