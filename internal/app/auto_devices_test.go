package app

import (
	"anykvm/internal/vnc"
	"gopkg.in/yaml.v3"
	"testing"
)

func TestAutoDeviceDefaultsAndExplicitDisabledRoundTrip(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		cfg := DefaultConfig()
		wire := "targets: [{video: {source: 'vnc://localhost'}}]"
		if disabled {
			wire = "targets: [{video: {source: 'vnc://localhost'}, audio: {source: ''}, hid: {device: ''}}]"
		}
		if err := yaml.Unmarshal([]byte(wire), &cfg); err != nil {
			t.Fatal(err)
		}
		want := "auto"
		if disabled {
			want = ""
		}
		if cfg.Targets[0].Audio.Source != want || cfg.Targets[0].HID.Device != want {
			t.Fatal("incorrect device defaults")
		}
		data, err := minimalConfigYAML(cfg)
		if err != nil {
			t.Fatal(err)
		}
		loaded := DefaultConfig()
		if err := yaml.Unmarshal(data, &loaded); err != nil {
			t.Fatal(err)
		}
		if loaded.Targets[0].HID.Device != want || loaded.Targets[0].Audio.Source != want {
			t.Fatal("saved device state changed")
		}
		machines := buildMachines(loaded)
		defer machines[0].Capture.Close()
		if disabled {
			if machines[0].HID != nil || machines[0].AudioDev != nil {
				t.Fatal("explicitly disabled devices enabled")
			}
		} else {
			remote, ok := machines[0].Capture.(*vnc.VNCClient)
			if !ok || machines[0].HID != remote || machines[0].AudioDev == nil {
				t.Fatal("Auto must share VNC video connection")
			}
		}
	}
}

func TestAutoDoesNotChooseUnrelatedHardware(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Targets[0].Video.Source = "unrelated-camera"
	machines := buildMachines(cfg)
	defer machines[0].Capture.Close()
	if machines[0].HID != nil || machines[0].AudioDev != nil {
		t.Fatal("Auto selected unrelated hardware")
	}
}

func TestScreenAutoAudio(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Targets[0].Video.Source = "screen"
	cfg.Targets[0].HID.Device = "" // Do not open local input in this audio test.
	machines := buildMachines(cfg)
	defer machines[0].Capture.Close()
	if machines[0].AudioDev == nil {
		t.Fatal("screen Auto audio must select the local default audio device")
	}
}
