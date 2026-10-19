//go:build darwin

package capture

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestScreenAudioNativeSampleBuffers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "screen-audio-test")
	args := []string{"clang", "-fblocks", "testdata/screen_audio_darwin.m", "-o", binary}
	for _, framework := range []string{"Foundation", "ScreenCaptureKit", "VideoToolbox", "CoreMedia", "CoreVideo", "CoreGraphics", "CoreImage", "AppKit"} {
		args = append(args, "-framework", framework)
	}
	if out, err := exec.CommandContext(ctx, "xcrun", args...).CombinedOutput(); err != nil {
		t.Fatalf("native audio fixture compile: %v\n%s", err, out)
	}
	if out, err := exec.CommandContext(ctx, binary).CombinedOutput(); err != nil {
		t.Fatalf("native audio fixture: %v\n%s", err, out)
	}
}

func TestScreenAudioPCM(t *testing.T) {
	values := []float32{-2, -1, -.5, 0, .5, 1, 2, float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))}
	want := []int16{-32768, -32768, -16384, 0, 16384, 32767, 32767, 0, 32767, -32768}
	data := make([]byte, len(values)*4)
	for i, v := range values {
		binary.LittleEndian.PutUint32(data[i*4:], math.Float32bits(v))
	}
	pcm := screenAudioPCM(data)
	for i, v := range want {
		if got := int16(binary.LittleEndian.Uint16(pcm[i*2:])); got != v {
			t.Fatalf("sample %d: %d != %d", i, got, v)
		}
	}
}

func TestScreenAudioSharesSessionAndLifecycle(t *testing.T) {
	d := newScreenDevice("screen").(*screenDevice)
	if d.audioRequested {
		t.Fatal("screen audio enabled without Auto")
	}
	a := d.AudioDevice().(*screenAudioDevice)
	if !d.audioRequested || a.screen != d {
		t.Fatal("audio does not share video device")
	}
	if err := a.Open(); !errors.Is(err, ErrStreamStopped) {
		t.Fatalf("open before video: %v", err)
	}
	if _, err := a.Capture(); !errors.Is(err, ErrStreamStopped) {
		t.Fatalf("capture before open: %v", err)
	}
	a.opened = true // Model the gap during a capture recovery reopen.
	if frame, err := a.Capture(); err != nil || frame != nil {
		t.Fatal("temporary reopen stopped audio", err)
	}
	a.Close()
	if _, err := a.Capture(); !errors.Is(err, ErrStreamStopped) {
		t.Fatalf("capture after close: %v", err)
	}
	if !d.audioRequested {
		t.Fatal("audio request lost across recovery")
	}
}

// Explicit opt-in only: this captures system audio in memory, never to disk.
func TestNativeScreenSystemAudio(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_SCREEN_AUDIO") != "1" {
		t.Skip("set ANYKVM_TEST_SCREEN_AUDIO=1 with screen/system-audio permission and audible playback")
	}
	d := newScreenDevice("screen").(*screenDevice)
	a := d.AudioDevice()
	if err := d.OpenWithFormat(640, 480, 30); err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := a.Open(); err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		frame, err := a.Capture()
		if err != nil {
			t.Fatal(err)
		}
		if frame != nil && len(frame.PCM) > 0 {
			if frame.SampleRate != 48000 || frame.Channels != 2 || len(frame.PCM)%4 != 0 {
				t.Fatal("unexpected PCM format")
			}
			for i := 0; i < len(frame.PCM); i += 2 {
				if binary.LittleEndian.Uint16(frame.PCM[i:]) != 0 {
					return
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no audible system PCM received; play sound in another app and check permissions")
}
