//go:build darwin

package capture

import (
	"os"
	"testing"
	"time"
)

// Explicit opt-in: temporarily switches the real display and always restores
// it. Optional screen capture stays in memory and checks geometry/letterboxing.
func TestNativeDesktopModeSwitchAndRestore(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_DESKTOP") != "1" {
		t.Skip("set ANYKVM_TEST_DESKTOP=1 to switch and restore a real display")
	}
	d := newScreenDevice("screen:0").(*screenDevice)
	defer func() {
		d.Close()
		if err := d.RestoreDesktopSize(); err != nil {
			t.Error(err)
		}
	}()
	d.mu.Lock()
	original, modes, err := d.displayModesLocked()
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	var target displayMode
	for _, mode := range modes {
		if mode.width != original.width && mode.width >= 1024 && mode.width <= 1920 && mode.height >= 600 && mode.height <= 1200 {
			target = mode
			break
		}
	}
	if target.width == 0 {
		t.Skip("no alternative usable display mode")
	}
	actual, err := d.SetDesktopSize(DesktopSize{Width: target.pixelWidth, Height: target.pixelHeight})
	if err != nil || actual.Width != target.pixelWidth || actual.Height != target.pixelHeight {
		t.Fatalf("switch: selected=%+v error=%v", actual, err)
	}
	current := func() displayMode {
		t.Helper()
		d.mu.Lock()
		defer d.mu.Unlock()
		mode, _, err := d.displayModesLocked()
		if err != nil {
			t.Fatal(err)
		}
		return mode
	}
	selected := current()
	if os.Getenv("ANYKVM_TEST_SCREEN") == "1" {
		d.SetVideoStreamOptions(&VideoStreamOptions{Width: 320, Height: 240, FPS: 30, Bitrate: 1000, Codec: "rawvideo"})
		if err := d.OpenWithFormat(0, 0, 30); err != nil {
			t.Fatal(err)
		}
		frame := desktopTestFrame(t, d)
		if frame.Width != 320 || frame.Height != 240 || frame.SourceWidth != selected.pixelWidth || frame.SourceHeight != selected.pixelHeight {
			t.Fatalf("capture geometry output=%dx%d source=%dx%d", frame.Width, frame.Height, frame.SourceWidth, frame.SourceHeight)
		}
		if selected.pixelWidth*240 > selected.pixelHeight*320 {
			for x := 0; x < 320; x++ {
				if frame.Data[x*4] != 0 || frame.Data[x*4+1] != 0 || frame.Data[x*4+2] != 0 {
					t.Fatal("letterbox row is not black")
				}
			}
		}
		d.Close()
		if after := current(); after.width != selected.width || after.height != selected.height {
			t.Fatal("capture close restored the display prematurely")
		}
	}
	if err := d.RestoreDesktopSize(); err != nil {
		t.Fatal(err)
	}
	after := current()
	if after.width != original.width || after.height != original.height || after.pixelWidth != original.pixelWidth || after.pixelHeight != original.pixelHeight || after.refreshRate != original.refreshRate {
		t.Fatalf("restore: got=%+v want=%+v", after, original)
	}
	if os.Getenv("ANYKVM_TEST_SCREEN") == "1" {
		d.SetVideoStreamOptions(nil)
		d.SetVideoCodec("rawvideo")
		if err := d.OpenWithFormat(0, 0, 30); err != nil {
			t.Fatal(err)
		}
		frame := desktopTestFrame(t, d)
		if frame.Width != original.pixelWidth || frame.Height != original.pixelHeight {
			t.Fatalf("native capture=%dx%d want=%dx%d", frame.Width, frame.Height, original.pixelWidth, original.pixelHeight)
		}
	}
	t.Logf("switched logical %dx%d -> %dx%d and restored; native pixels=%dx%d", original.width, original.height, selected.width, selected.height, original.pixelWidth, original.pixelHeight)
}

func desktopTestFrame(t *testing.T, d *screenDevice) *Frame {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		frame, err := d.Capture()
		if err != nil {
			t.Fatal(err)
		}
		if frame != nil {
			return frame
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("no captured frame")
	return nil
}

func TestNativeDesktopDPISwitchAndRestore(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_DESKTOP") != "1" {
		t.Skip("set ANYKVM_TEST_DESKTOP=1 to switch and restore real display DPI")
	}
	d := newScreenDevice("screen:0").(*screenDevice)
	defer func() {
		if err := d.RestoreDesktopSize(); err != nil {
			t.Error(err)
		}
	}()
	d.mu.Lock()
	original, modes, err := d.displayModesLocked()
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if original.pixelHeight < 1080 {
		t.Skip("DPI switching requires output height at least 1080")
	}
	for _, percent := range []int{100, 200} {
		request := DesktopSize{Width: original.pixelWidth, Height: original.pixelHeight, ScalePercent: percent}
		want, ok := nearestDisplayMode(modes, request, original)
		if !ok || want.width*percent != original.pixelWidth*100 || want.height*percent != original.pixelHeight*100 || want.pixelWidth != original.pixelWidth || want.pixelHeight != original.pixelHeight {
			t.Skip("display lacks exact 100% and 200% modes with equal backing pixels")
		}
	}
	for _, percent := range []int{100, 200} {
		actual, err := d.SetDesktopSize(DesktopSize{Width: original.pixelWidth, Height: original.pixelHeight, ScalePercent: percent})
		if err != nil || actual.Width != original.pixelWidth || actual.Height != original.pixelHeight {
			t.Fatalf("DPI switch=%+v error=%v", actual, err)
		}
		d.mu.Lock()
		mode, _, err := d.displayModesLocked()
		d.mu.Unlock()
		if err != nil || mode.width*percent != original.pixelWidth*100 || mode.height*percent != original.pixelHeight*100 {
			t.Fatalf("scale=%d mode=%+v error=%v", percent, mode, err)
		}
	}
	// Reproduce the reported client request, including a conflicting DPI value.
	actual, err := d.SetDesktopSize(DesktopSize{Width: 1532, Height: 1007, ScalePercent: 200})
	if err != nil || actual.ScalePercent != 100 {
		t.Fatalf("1532x1007 native scale protection: selected=%+v error=%v", actual, err)
	}
	if err := d.RestoreDesktopSize(); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	after, _, err := d.displayModesLocked()
	d.mu.Unlock()
	if err != nil || after.width != original.width || after.height != original.height || after.pixelWidth != original.pixelWidth || after.pixelHeight != original.pixelHeight {
		t.Fatalf("DPI restoration=%+v want=%+v error=%v", after, original, err)
	}
	t.Logf("100%% and 200%% DPI switches retained %dx%d backing pixels and restored %dx%d points", original.pixelWidth, original.pixelHeight, original.width, original.height)
}
