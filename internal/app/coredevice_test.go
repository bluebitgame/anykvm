package app

import (
	"anykvm/internal/bridge"
	"anykvm/internal/capture"
	"anykvm/internal/coredevice"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCoreDeviceConfigAndDirectSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := `vnc: {addr: ':5900'}
targets:
  - name: iPhone
    video: {source: 'coredevice:test-phone'}
    coredevice:
      rsd: '[::1]:58783'
      timeout: 5s
    hid: {mouse_mode: absolute}
`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Targets[0]
	if m.CoreDevice.Timeout != 5*time.Second || m.CoreDevice.RSD != "[::1]:58783" {
		t.Fatal("CoreDevice options lost")
	}
	d, android := newCaptureSession(m)
	if _, ok := d.(*coredevice.Client); !ok || android != nil {
		t.Fatal("direct CoreDevice backend not selected")
	}
	if _, ok := d.(bridge.HIDSender); !ok {
		t.Fatal("capture must supply input")
	}
	stream, ok := d.(capture.VideoStreamConfigurer)
	if !ok {
		t.Fatal("CoreDevice must forward native GameStream video")
	}
	stream.SetVideoStreamOptions(&capture.VideoStreamOptions{Codec: "hevc"})
	b := bridge.NewBridge([]*bridge.MachineSession{{Capture: d}}, "alt", 0, 0, 0)
	if !b.SupportsGameStreamHEVC(0) || b.SupportsGameStreamAV1(0) {
		t.Fatal("incorrect CoreDevice GameStream codec advertisement")
	}
	for _, codec := range []string{"", "h264"} {
		if err := b.StartGameStream(0, capture.VideoStreamOptions{Codec: codec}); err == nil {
			t.Fatal("CoreDevice accepted an unsupported GameStream codec")
		}
	}
	if err := checkCurrentServerConfig([]byte(data)); err != nil {
		t.Fatal(err)
	}
	m.HID.Device = "ble:ttyACM0:0"
	if err := validateCoreDeviceMachine(m); err == nil {
		t.Fatal("external HID silently overrides direct control")
	}
	m.HID.Device = ""
	m.Video.Source = "coredevice:"
	d, _ = newCaptureSession(m)
	if err := d.Open(); err == nil {
		t.Fatal("invalid CoreDevice target became a local camera")
	}
}
