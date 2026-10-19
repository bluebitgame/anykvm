package capture

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestLibraryH264Capture(t *testing.T) {
	t.Run("GameStream", func(t *testing.T) { testH264CaptureFrames(t, true) })
	t.Run("WebVNC", func(t *testing.T) { testH264CaptureFrames(t, false) })
}

func testH264CaptureFrames(t *testing.T, direct bool) {
	ff := os.Getenv("ANYKVM_TEST_FFMPEG")
	if os.Getenv("ANYKVM_TEST_AVMEDIA") == "" || ff == "" {
		t.Skip("set ANYKVM_TEST_AVMEDIA, ANYKVM_FFMPEG_LIBDIR and ANYKVM_TEST_FFMPEG")
	}
	profile := VideoStreamOptions{Width: 320, Height: 240, FPS: 30, Bitrate: 1000}
	d := &ffmpegDevice{videoCodec: "h264", streamOptions: &profile, scalableInput: true, reqFPS: 30}
	// The source is read by libavdevice; CLI output arguments are not involved.
	args := []string{"-f", "lavfi", "-i", "testsrc2=size=320x200:rate=30"}
	height := 240
	if !direct {
		d.streamOptions = nil
		height = 200
	}
	if err := d.openArgs("", args, 320, height); err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var encoded bytes.Buffer
	deadline := time.Now().Add(5 * time.Second)
	for i := 0; i < 20; {
		if time.Now().After(deadline) {
			t.Fatal("capture did not produce 20 frames")
		}
		time.Sleep(time.Second / 30)
		f, err := d.Capture()
		if err != nil {
			t.Fatal(err)
		}
		if f == nil {
			continue
		}
		if f.Format != FormatH264 || f.Width != 320 || f.Height != height || f.SourceWidth != 320 || f.SourceHeight != 200 {
			t.Fatalf("unexpected captured frame: %+v", f)
		}
		encoded.Write(f.Data)
		f.Release()
		i++
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ff, "-hide_banner", "-loglevel", "error", "-xerror", "-f", "h264", "-i", "pipe:0", "-f", "rawvideo", "-pix_fmt", "rgb24", "pipe:1")
	cmd.Stdin = &encoded
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v: %s", err, &stderr)
	}
	size := 320 * height * 3
	if len(raw) != 20*size {
		t.Fatalf("decoded %d bytes", len(raw))
	}
	if bytes.Equal(raw[:size], raw[19*size:]) {
		t.Fatal("capture did not update")
	}
}

func TestAUDParserPreservesMultipleSlices(t *testing.T) {
	nal := func(kind byte) []byte { return []byte{0, 0, 1, kind, 0xaa} }
	first := append(nal(9), nal(7)...)
	first = append(first, nal(8)...)
	first = append(first, nal(5)...)
	first = append(first, nal(5)...)
	stream := append(append([]byte(nil), first...), nal(9)...)
	stream = append(stream, nal(1)...)
	p := &AUParser{audDelimited: true}
	var frames [][]byte
	for _, b := range stream {
		frames = append(frames, p.Push([]byte{b})...)
	}
	if len(frames) != 1 || !bytes.Equal(frames[0], first) {
		t.Fatalf("split multi-slice frame: %x", frames)
	}
	if p.FlushPending() != nil {
		t.Fatal("idle flush emitted incomplete AUD frame")
	}
}
