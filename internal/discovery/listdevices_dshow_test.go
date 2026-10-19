package discovery

import (
	"anykvm/internal/capture"
	"reflect"
	"testing"
)

func TestDirectShowDeviceListing(t *testing.T) {
	lines := []string{
		`"USB Capture" (video)`,
		`  Alternative name "@device_pnp_fake"`,
		`"麦克风 (USB)" (audio)`,
		`"Combined device" (video, audio)`,
		`"Unknown" (none)`,
		`Could not enumerate devices.`,
	}
	for _, tc := range []struct {
		kind string
		want []string
	}{
		{"video", []string{"USB Capture", "Combined device"}},
		{"audio", []string{"麦克风 (USB)", "Combined device"}},
	} {
		if got := directShowDevices(lines, tc.kind); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s: %v", tc.kind, got)
		}
	}
}

func TestScreenOptions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		screens []capture.Screen
		want    []DeviceOption
	}{
		{"unknown", nil, []DeviceOption{{Value: "screen", Label: "Local screen"}}},
		{"one", []capture.Screen{{Index: 0, Width: 1920, Height: 1080}}, []DeviceOption{{Value: "screen", Label: "Local screen (1920×1080)"}}},
		{"two", []capture.Screen{{Index: 0, Width: 1920, Height: 1080}, {Index: 1, Width: 2560, Height: 1440}}, []DeviceOption{{Value: "screen:0", Label: "Display 1 (1920×1080)"}, {Value: "screen:1", Label: "Display 2 (2560×1440)"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := screenOptions(tc.screens); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("screen options = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestDisplayDeviceName(t *testing.T) {
	for input, want := range map[string]string{
		"wasapi:":         "Default",
		"wasapi:Speakers": "Speakers",
		"Microphone":      "Microphone",
	} {
		if got := displayDeviceName(input); got != want {
			t.Fatalf("display device %q = %q, want %q", input, got, want)
		}
	}
}

func TestDeviceSection(t *testing.T) {
	lines := []string{
		"[AVFoundation indev @ 0x0] AVFoundation video devices:",
		"[AVFoundation indev @ 0x0] [1] Second camera",
		"[AVFoundation indev @ 0x0] [0] First camera",
		"[AVFoundation indev @ 0x0] AVFoundation audio devices:",
	}
	if got, want := deviceSection(lines, "AVFoundation video devices:"), []string{"First camera", "Second camera"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("devices = %v, want %v", got, want)
	}
}
