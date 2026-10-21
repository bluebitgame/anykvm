package app

import (
	"fmt"
	"log"
	"runtime"

	"anykvm/internal/airplay"
	"anykvm/internal/ard"
	"anykvm/internal/bridge"
	"anykvm/internal/capture"
	"anykvm/internal/coredevice"
	"anykvm/internal/hid"
	"anykvm/internal/rdp"
	"anykvm/internal/rtsp"
	"anykvm/internal/scrcpy"
	"anykvm/internal/spice"
	"anykvm/internal/vnc"
)

func buildMachines(cfg Config) []*bridge.MachineSession {
	var machines []*bridge.MachineSession

	for i, mc := range cfg.Targets {
		ms := &bridge.MachineSession{Name: mc.Name}
		if ms.Name == "" {
			ms.Name = fmt.Sprintf("Target-%d", i+1)
		}

		cw, ch, fps := capture.ParseResolution(mc.Video.Mode)
		ms.FPS = fps
		ms.CapW = cw
		ms.CapH = ch
		ms.AutoResolution = capture.ResolutionFollowsClient(mc.Video.Mode)
		ms.CursorRendering = mc.Video.CursorRendering

		dev, androidSess := newCaptureSession(mc)
		if err := capture.ConfigureMode(dev, mc.Video.Mode); err != nil {
			log.Printf("AnyKVM: target %q video.mode: %v", ms.Name, err)
			dev = capture.Failed(err)
		}
		if screen, ok := dev.(interface{ SetHideCursor(bool) }); ok {
			screen.SetHideCursor(mc.Video.CursorRendering != "remote")
		}
		ms.Capture = dev

		ms.FallbackW = cw
		ms.FallbackH = ch
		if ms.FallbackW <= 0 || ms.FallbackH <= 0 {
			ms.FallbackW = defaultWidth
			ms.FallbackH = defaultHeight
		}

		// HID.
		hidCfg := mc.HID
		if hidCfg.Device == "auto" && capture.IsScreenDevice(mc.Video.Source) {
			hidCfg.Device = "local"
		}
		spec := hid.SpecOrAuto(hidCfg.Device)
		hidType := spec.Type
		devPath := spec.Device
		profileSlot := spec.Slot
		if profileSlot < 0 {
			profileSlot = 0
		}

		mouseMode := hidCfg.MouseMode
		if mouseMode == "" {
			mouseMode = "absolute"
		}
		ms.MouseMode = mouseMode
		ms.WheelScale = mc.HID.WheelScale

		_, sourceHID := dev.(bridge.HIDSender)
		if hidCfg.Device == "" || (hidCfg.Device == "auto" && androidSess == nil && !sourceHID) {
			log.Printf("AnyKVM: target %q — HID disabled", ms.Name)
		} else if androidSess != nil {
			ms.HID = androidSess
		} else if hs, ok := dev.(bridge.HIDSender); ok {
			ms.HID = hs
			log.Printf("AnyKVM: target %q — capture-provided HID", ms.Name)
		} else if hidType == "local" {
			if h, err := hid.OpenLocal(); err == nil {
				ms.HID = h
				log.Printf("AnyKVM: target %q — local input", ms.Name)
			} else {
				log.Printf("AnyKVM: target %q — local input unavailable: %v", ms.Name, err)
			}
		} else {
			slot := hid.SlotUnset
			if hidType == "ble" || hidType == "nrf52840" {
				slot = profileSlot
			}
			ms.HID = hid.NewSessionDevice(hidType, devPath, mouseMode, slot)
			log.Printf("AnyKVM: target %q — HID ready on demand (%s/%s, mouse=%s)", ms.Name, hidType, devPath, mouseMode)
		}

		// Audio.
		if mc.Audio.Source == "auto" {
			if ad, ok := dev.(capture.AudioProvider); ok {
				ms.AudioDev = ad.AudioDevice()
			}
			if ms.AudioDev == nil && capture.IsScreenDevice(mc.Video.Source) {
				device := ""
				if runtime.GOOS == "windows" {
					device = "wasapi"
				}
				ms.AudioDev = capture.NewAudioDevice(device)
			}
		} else if mc.Audio.Source != "" {
			ms.AudioDev = capture.NewAudioDevice(mc.Audio.Source)
		}

		machines = append(machines, ms)
	}
	return machines
}

func newCaptureSession(mc TargetConfig) (capture.Device, *scrcpy.Session) {
	if dev, ok := spice.NewCapture(mc.Video.Source, mc.SPICE); ok {
		return dev, nil
	}
	if dev, ok := rtsp.NewCapture(mc.Video.Source, mc.RTSP); ok {
		return dev, nil
	}
	if dev, ok := rdp.NewSource(mc.Video.Source, mc.RDP); ok {
		return dev, nil
	}
	if dev, ok := coredevice.NewCapture(mc.Video.Source, mc.HID.Device, mc.CoreDevice); ok {
		return dev, nil
	}
	if dev, ok := ard.NewCapture(mc.Video.Source, mc.ARD); ok {
		return dev, nil
	}
	if dev, ok := airplay.NewCapture(mc.Video.Source, mc.AirPlay); ok {
		return dev, nil
	}
	if vnc.IsVNCDevice(mc.Video.Source) {
		dev, err := vnc.NewVNCClient(mc.Video.Source)
		if err != nil {
			log.Printf("AnyKVM: VNC device error: %v", err)
			return capture.NewDevice(mc.Video.Source), nil
		}
		dev.SetWakeOnLAN(mc.WoL)
		log.Printf("AnyKVM: VNC capture device %v", mc.Video.Source)
		return dev, nil
	}
	if !scrcpy.IsAndroidDevice(mc.Video.Source) {
		return capture.NewDevice(mc.Video.Source), nil
	}
	sess := scrcpy.NewSession(scrcpy.Options{
		Serial:                scrcpy.DeviceSerial(mc.Video.Source),
		BitRate:               mc.Scrcpy.BitRate,
		ServerJar:             mc.Scrcpy.Server,
		Version:               mc.Scrcpy.Version,
		StayAwake:             !mc.Scrcpy.KeepActive,
		KeepActive:            mc.Scrcpy.KeepActive,
		Audio:                 mc.Audio.Source == "auto",
		Crop:                  mc.Scrcpy.Crop,
		GameStreamOrientation: mc.Scrcpy.GameStreamOrientation,
	})
	return scrcpy.NewCapture(sess), sess
}
