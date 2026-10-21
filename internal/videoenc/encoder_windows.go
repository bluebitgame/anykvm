//go:build windows && cgo

package videoenc

import (
	"golang.org/x/sys/windows/registry"
	"strings"
)

func newPlatformEncoder(o Options) (Encoder, error) {
	if o.Codec != "h264" && o.Codec != "hevc" && o.Codec != "av1" {
		return nil, ErrUnavailable
	}
	return selectWindowsEncoder(o, windowsHardwareCodecs(o.Codec), openWindowsVendor, openMediaFoundation, openAV)
}

func windowsHardwareCodecs(codec string) []string {
	var hardware []string
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Enum\PCI`, registry.ENUMERATE_SUB_KEYS)
	if err == nil {
		names, _ := key.ReadSubKeyNames(-1)
		key.Close()
		for _, vendor := range []struct{ id, codec string }{{"VEN_10DE", codec + "_nvenc"}, {"VEN_1002", codec + "_amf"}, {"VEN_8086", codec + "_qsv"}} {
			for _, name := range names {
				if strings.Contains(strings.ToUpper(name), vendor.id) && isDisplayDevice(name) {
					hardware = append(hardware, vendor.codec)
					break
				}
			}
		}
	}
	return hardware
}

func isDisplayDevice(name string) bool {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Enum\PCI\`+name, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return false
	}
	defer key.Close()
	instances, _ := key.ReadSubKeyNames(-1)
	for _, instance := range instances {
		entry, err := registry.OpenKey(key, instance, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		guid, _, err := entry.GetStringValue("ClassGUID")
		entry.Close()
		if err == nil && strings.EqualFold(guid, "{4d36e968-e325-11ce-bfc1-08002be10318}") {
			return true
		}
	}
	return false
}
