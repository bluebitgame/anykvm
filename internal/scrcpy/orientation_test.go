package scrcpy

import (
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"

	"anykvm/internal/capture"
)

func TestGameStreamOrientationLifecycle(t *testing.T) {
	sess := NewSession(Options{GameStreamOrientation: "@90"})
	dev := NewCapture(sess)
	check := func(want string) {
		t.Helper()
		sess.mu.Lock()
		args := sess.serverArgs("3.3.4", "12345678", 1280, 30)
		sess.mu.Unlock()
		orientation := ""
		for _, arg := range args {
			if strings.HasPrefix(arg, "capture_orientation=") {
				orientation = strings.TrimPrefix(arg, "capture_orientation=")
			}
		}
		if orientation != want {
			t.Fatalf("server invocation orientation = %q, want %q", orientation, want)
		}
	}
	check("") // Ordinary capture keeps Android's orientation.
	dev.SetGameStreamCaptureOptions(&capture.VideoStreamOptions{})
	check("@90")
	if err := dev.Close(); err != nil {
		t.Fatal(err)
	}
	check("")
	dev.SetGameStreamCaptureOptions(&capture.VideoStreamOptions{}) // Reopening for IDR reapplies the override.
	check("@90")
	dev.SetGameStreamCaptureOptions(nil) // Web/VNC takeover restores normal capture.
	check("")
}

func TestCaptureOrientationValidation(t *testing.T) {
	for _, value := range []string{"", "@", "0", "90", "180", "270", "flip0", "flip90", "flip180", "flip270", "@0", "@90", "@180", "@270", "@flip0", "@flip90", "@flip180", "@flip270"} {
		if err := ValidateCaptureOrientation(value); err != nil {
			t.Errorf("rejected scrcpy orientation %q: %v", value, err)
		}
	}
	for _, value := range []string{"45", "-90", "360", "auto", "@@90", "90 audio=true", "90;echo bad"} {
		if err := ValidateCaptureOrientation(value); err == nil {
			t.Errorf("accepted invalid orientation %q", value)
		}
	}
}

// scrcpy-server performs the inverse crop/rotation. Control messages must remain
// in encoded-video coordinates, without adding the crop offset in AnyKVM.
func TestCroppedRotatedVideoControlCoordinates(t *testing.T) {
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()
	sess := NewSession(Options{Crop: "1272:2261:0:511", GameStreamOrientation: "270"})
	sess.control, sess.running = local, true
	sess.setSize(1280, 720)
	done := make(chan error, 1)
	go func() { done <- sess.MouseMove(32767, 0) }()
	packet := make([]byte, 32)
	if _, err := io.ReadFull(remote, packet); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if binary.BigEndian.Uint32(packet[10:]) != 1279 || binary.BigEndian.Uint32(packet[14:]) != 0 || binary.BigEndian.Uint16(packet[18:]) != 1280 || binary.BigEndian.Uint16(packet[20:]) != 720 {
		t.Fatalf("touch coordinates did not match rotated video: %x", packet)
	}
}
