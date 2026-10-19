package scrcpy

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"os"
	"strings"
	"testing"

	"anykvm/internal/capture"
)

func TestHEVCPassthroughAndRotation(t *testing.T) {
	data, err := os.ReadFile("../testdata/passthrough.hevc")
	if err != nil {
		t.Fatal(err)
	}
	if w, h, ok := parseHEVCSPSSize(data); !ok || w != 64 || h != 48 {
		t.Fatalf("HEVC dimensions=%dx%d valid=%v", w, h, ok)
	}
	for _, bad := range [][]byte{nil, {0, 0, 1, 0x42, 1}, {0, 0, 1, 0x42, 1, 0xff, 0xff}} {
		if _, _, ok := parseHEVCSPSSize(bad); ok {
			t.Fatalf("accepted truncated HEVC SPS: %x", bad)
		}
	}
	for _, modern := range []bool{false, true} {
		var stream bytes.Buffer
		flag := uint32(0x80000000)
		if modern {
			flag = 0x40000000
		}
		writeWords(&stream, flag, 0, uint32(len(data)))
		stream.Write(data)
		writeWords(&stream, 0, 123, 4)
		stream.Write([]byte{0, 0, 1, 0x26})
		sess := NewSession(Options{})
		sess.vr, sess.sessionPackets = bufio.NewReader(&stream), modern
		sess.setSize(1920, 1080)
		c := NewCapture(sess)
		c.activeCodec = "hevc"
		f, err := c.Capture()
		want := append(bytes.Clone(data), 0, 0, 1, 0x26)
		if err != nil || f == nil || f.Format != capture.FormatHEVC || f.Width != 64 || f.Height != 48 || !bytes.Equal(f.Data, want) {
			t.Fatalf("HEVC passthrough frame=%+v err=%v", f, err)
		}
	}
}

func TestScrcpyEncoderSelection(t *testing.T) {
	sess := NewSession(Options{Audio: true, KeepActive: true})
	c := NewCapture(sess)
	if !c.SupportsVideoCodec("hevc") || !c.SupportsGameStreamHEVC() || c.SupportsVideoCodec("av1") {
		t.Fatal("incorrect codec capabilities")
	}
	c.SetVideoStreamOptions(&capture.VideoStreamOptions{Codec: "hevc"})
	if c.codec != "hevc" {
		t.Fatal("GameStream HEVC was not selected")
	}
	// A nil GameStream profile must not undo the browser's HEVC selection.
	c.SetVideoStreamOptions(nil)
	if c.codec != "hevc" {
		t.Fatal("browser HEVC selection was overwritten")
	}
	c.SetVideoStreamOptions(&capture.VideoStreamOptions{})
	if c.codec != "h264" {
		t.Fatal("GameStream default must restore H.264")
	}
	sess.videoCodec = "h265"
	args := strings.Join(sess.serverArgs("5.0.1", "12345678", 1280, 30), " ")
	for _, arg := range []string{"audio=true", "audio_codec=raw", "keep_active=true", "video_codec=h265"} {
		if !strings.Contains(args, arg) {
			t.Fatalf("missing %s in %s", arg, args)
		}
	}
	if strings.Contains(args, "new_display=") || strings.Contains(args, "flex_display=") {
		t.Fatal("independent desktop unexpectedly enabled")
	}
}

func TestVideoResetPreservesAudio(t *testing.T) {
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()
	sess := NewSession(Options{Audio: true})
	sess.control, sess.running = local, true
	frames := make(chan *capture.AudioFrame, 1)
	sess.audioFrames = frames
	done := make(chan error, 1)
	go func() { done <- NewCapture(sess).RequestKeyframe() }()
	var command [1]byte
	if _, err := io.ReadFull(remote, command[:]); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil || command[0] != 17 || sess.audioFrames != frames || !sess.running {
		t.Fatalf("video reset disrupted session: command=%d err=%v", command[0], err)
	}
}
