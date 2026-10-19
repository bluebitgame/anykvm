package discovery

import (
	"anykvm/internal/avmedia"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"sort"
	"strings"

	"anykvm/internal/capture"
	"anykvm/internal/coredevice"
	"anykvm/internal/hid"
)

// DeviceOption pairs a value accepted by the configuration with a display name.
type DeviceOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// ConfigDevices returns built-in and discovered capture and HID choices.
// The UI must also accept custom values for remote sources such as VNC, ARD,
// and AirPlay, which cannot be discovered locally.
func ConfigDevices() map[string][]DeviceOption {
	return cachedDevices(false)
}

func discoverDevices(includeIOS bool) map[string][]DeviceOption {
	video := screenOptions(capture.ListScreens())
	for _, device := range capture.ListNativeVideoDevices(includeIOS) {
		video = appendOption(video, device, device)
	}

	audio := make([]DeviceOption, 0)
	for _, device := range capture.ListWASAPILoopbackDevices() {
		audio = appendOption(audio, device, displayDeviceName(device))
	}
	for _, device := range capture.ListNativeAudioDevices() {
		audio = appendOption(audio, device, displayDeviceName(device))
	}
	if ff, ok := capture.ResolveFFmpeg(); ok {
		lines := runFFProbe(ff)
		if runtime.GOOS == "windows" {
			for _, device := range directShowDevices(lines, "video") {
				video = appendOption(video, device, device)
			}
			for _, device := range directShowDevices(lines, "audio") {
				audio = appendOption(audio, device, displayDeviceName(device))
			}
		} else {
			for _, device := range deviceSection(lines, "AVFoundation video devices:") {
				video = appendOption(video, device, device)
			}
			for _, device := range deviceSection(lines, "AVFoundation audio devices:") {
				audio = appendOption(audio, device, displayDeviceName(device))
			}
		}
	}

	if includeIOS {
		selectors, err := coredevice.ListUSBDevices()
		if err != nil {
			log.Printf("CoreDevice USB discovery: %v", err)
		} else {
			for _, selector := range selectors {
				video = appendOption(video, selector, selector)
			}
		}
	}

	hidOptions := []DeviceOption{{Value: "local", Label: "Local input"}}
	if gadget, _ := hid.ListGadget(); gadget != "" {
		hidOptions = appendOption(hidOptions, "gadget", "USB gadget")
	}
	if bluetooth := hid.ListBluetooth(); len(bluetooth) != 0 {
		hidOptions = appendOption(hidOptions, "bluetooth", "Bluetooth HID")
	}
	blePorts := make(map[string]struct{})
	for _, device := range hid.ListBLE() {
		blePorts[device.Port] = struct{}{}
		for slot := 0; slot < device.SlotCount; slot++ {
			label := fmt.Sprintf("%s %s — slot %d", device.Name, device.Port, slot+1)
			hidOptions = appendOption(hidOptions, fmt.Sprintf("ble:%s:%d", device.Port, slot), label)
		}
	}
	for _, port := range listSerialPorts() {
		if _, isBLE := blePorts[port]; isBLE {
			continue
		}
		if hid.ProbeCH9329(port) {
			hidOptions = appendOption(hidOptions, "ch9329:"+port, "CH9329 HID "+port)
			continue
		}
		if hid.ProbeHIDBridge(port) {
			hidOptions = appendOption(hidOptions, "hid:"+port, "HID "+port)
		}
	}

	return map[string][]DeviceOption{
		"video": video,
		"audio": audio,
		"hid":   hidOptions,
	}
}

func screenOptions(screens []capture.Screen) []DeviceOption {
	if len(screens) == 1 {
		return []DeviceOption{{Value: "screen", Label: screenLabel("Local screen", screens[0])}}
	}
	if len(screens) > 1 {
		options := make([]DeviceOption, 0, len(screens))
		for _, screen := range screens {
			options = append(options, DeviceOption{Value: fmt.Sprintf("screen:%d", screen.Index), Label: screenLabel(fmt.Sprintf("Display %d", screen.Index+1), screen)})
		}
		return options
	}
	return []DeviceOption{{Value: "screen", Label: "Local screen"}}
}

func screenLabel(name string, screen capture.Screen) string {
	if screen.Width > 0 && screen.Height > 0 {
		return fmt.Sprintf("%s (%d×%d)", name, screen.Width, screen.Height)
	}
	return name
}

func displayDeviceName(device string) string {
	if device == "wasapi:" {
		return "Default"
	}
	return strings.TrimPrefix(device, "wasapi:")
}

func appendOption(options []DeviceOption, value, label string) []DeviceOption {
	if value == "" {
		return options
	}
	for _, option := range options {
		if option.Value == value {
			return options
		}
	}
	return append(options, DeviceOption{Value: value, Label: label})
}

// List prints the same cached discovery result used by the Web configuration.
// includeIOS additionally discovers USB mirrors and CoreDevice selectors.
func List(includeIOS bool) {
	printDevices(os.Stdout, cachedDevices(includeIOS))
}

func printDevices(w io.Writer, devices map[string][]DeviceOption) {
	for _, section := range []struct{ kind, title string }{
		{"video", "video capture devices"}, {"audio", "audio input devices"}, {"hid", "HID devices"},
	} {
		fmt.Fprintf(w, "=== %s ===\n", section.title)
		for _, device := range devices[section.kind] {
			if device.Label == "" || device.Label == device.Value {
				fmt.Fprintf(w, "  %s\n", device.Value)
			} else {
				fmt.Fprintf(w, "  %s — %s\n", device.Value, device.Label)
			}
		}
		fmt.Fprintln(w)
	}
}

// runFFProbe uses in-process device enumeration; no executable is invoked.
func runFFProbe(_ string) []string {
	if runtime.GOOS == "windows" {
		return avmedia.DeviceLog("dshow")
	}
	return avmedia.DeviceLog("avfoundation")
}

func directShowDevices(lines []string, kind string) []string {
	var devices []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "\"") {
			continue
		}
		end := strings.LastIndex(line, "\" (")
		if end < 1 || !strings.HasSuffix(line, ")") {
			continue
		}
		for _, media := range strings.Split(line[end+3:len(line)-1], ",") {
			if strings.TrimSpace(media) == kind {
				devices = append(devices, line[1:end])
				break
			}
		}
	}
	return devices
}
func deviceSection(lines []string, searchHeader string) []string {
	inSection := false
	entries := make(map[int]string)
	for _, line := range lines {
		if strings.Contains(line, searchHeader) {
			inSection = true
			continue
		}
		if !inSection {
			continue
		}
		if line != "" && strings.Contains(line, "devices:") {
			break
		}
		idx, name := parseDeviceLine(line)
		if idx >= 0 {
			entries[idx] = name
		}
	}
	if len(entries) == 0 {
		return nil
	}
	keys := make([]int, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	devices := make([]string, 0, len(keys))
	for _, k := range keys {
		devices = append(devices, entries[k])
	}
	return devices
}

// parseDeviceLine extracts "[index] Name" from an ffmpeg AVFoundation device-line.
func parseDeviceLine(line string) (int, string) {
	start := strings.Index(line, "] [")
	if start < 0 {
		return -1, ""
	}
	rest := line[start+3:]
	end := strings.IndexByte(rest, ']')
	if end < 0 {
		return -1, ""
	}
	idx := 0
	fmt.Sscanf(rest[:end], "%d", &idx)
	name := strings.TrimSpace(rest[end+1:])
	return idx, name
}

// listSerialPorts returns a descriptive list of accessible serial ports,
// used for CH9329 and HID controllers.
func listSerialPorts() []string {
	return platformSerialPorts()
}
