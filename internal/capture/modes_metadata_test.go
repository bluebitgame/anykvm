package capture

import (
	"errors"
	"fmt"
	"testing"
)

func TestWindowsModeMetadata(t *testing.T) {
	data := []byte(`[
 {"width":1920,"height":1080,"pixelFormat":"nv12","minFPS":59.9400599401,"maxFPS":59.9400599401},
 {"width":1920,"height":1080,"pixelFormat":"nv12","minFPS":59.9400599401,"maxFPS":59.9400599401},
 {"width":1920,"height":1080,"pixelFormat":"mjpeg","minFPS":30,"maxFPS":60},
 {"width":1280,"height":720,"pixelFormat":"fourcc_30323449","minFPS":30,"maxFPS":30},
 {"width":640,"height":480,"pixelFormat":"guid_123456780001000280001234567890ab","minFPS":25,"maxFPS":25},
 {"width":0,"height":1080,"pixelFormat":"nv12","minFPS":30,"maxFPS":60},
 {"width":1920,"height":1080,"pixelFormat":"nv12","minFPS":60,"maxFPS":30},
 {"width":1920,"height":1080,"pixelFormat":"fake","minFPS":30,"maxFPS":60}
 ]`)
	modes, err := decodeDeviceVideoModes(data)
	if err != nil || len(modes) != 4 {
		t.Fatalf("modes=%+v err=%v", modes, err)
	}
	if modes[0].PixelFormat != "mjpeg" || modes[1].MaxFPS != 59.9400599401 {
		t.Fatalf("modes lost ordering or rational cadence: %+v", modes)
	}
	for _, mode := range modes {
		value := fmt.Sprintf("%dx%d@%.3f:%s", mode.Width, mode.Height, mode.MaxFPS, mode.PixelFormat)
		parsed, err := ParseMode(value)
		if err != nil || parsed.PixelFormat != mode.PixelFormat {
			t.Fatalf("discovered mode cannot be configured: %s %v", value, err)
		}
	}
	for _, bad := range []string{`[]`, `null`, `{}`, `[{"width":1920,"height":1080,"pixelFormat":"nv12","minFPS":0,"maxFPS":60}]`} {
		if _, err := decodeDeviceVideoModes([]byte(bad)); err == nil {
			t.Fatalf("accepted unusable metadata %s", bad)
		}
	}
}

func TestWindowsModesRemainAvailableDuringCapture(t *testing.T) {
	var cache deviceModeCache
	calls := 0
	discover := func() ([]VideoMode, error) { calls++; return []VideoMode{{1920, 1080, "nv12", 60, 60}}, nil }
	if _, err := cache.list("card", discover); err != nil {
		t.Fatal(err)
	}
	cache.use("card", 1)
	locked := func() ([]VideoMode, error) { return nil, errors.New("device in use") }
	modes, err := cache.list("card", locked)
	if err != nil || len(modes) != 1 || calls != 1 {
		t.Fatal("active capture lost capabilities", err)
	}
	modes[0].Width = 1
	modes, _ = cache.list("card", locked)
	if modes[0].Width != 1920 {
		t.Fatal("caller mutated cached modes")
	}
	cache.use("card", -1)
	if _, err := cache.list("card", locked); err == nil {
		t.Fatal("inactive input returned stale capabilities")
	}
	if _, err := cache.list("card", discover); err != nil || calls != 2 {
		t.Fatal("input was not re-enumerated", err)
	}
}
