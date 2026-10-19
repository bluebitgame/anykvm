package videoenc

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// linuxH264Hardware reads sysfs without starting a driver utility or encoder.
// NVIDIA cards need not expose a DRM render node, so discover them through PCI.
// VAAPI is restricted to render nodes belonging to AMD or Intel; an unreadable
// or unknown vendor is not evidence that a backend should be attempted.
func linuxH264Hardware(sysRoot, driRoot string) (nvidia bool, vaapi []string) {
	pci, _ := filepath.Glob(filepath.Join(sysRoot, "bus/pci/devices/*"))
	for _, device := range pci {
		vendor, vendorOK := readSysfsHex(filepath.Join(device, "vendor"))
		class, classOK := readSysfsHex(filepath.Join(device, "class"))
		if vendorOK && classOK && vendor == 0x10de && class>>16 == 0x03 {
			nvidia = true
		}
	}
	nodes, _ := filepath.Glob(filepath.Join(driRoot, "renderD*"))
	for _, node := range nodes {
		vendor, ok := readSysfsHex(filepath.Join(sysRoot, "class/drm", filepath.Base(node), "device/vendor"))
		if !ok {
			continue
		}
		switch vendor {
		case 0x10de:
			nvidia = true
		case 0x1002, 0x8086:
			vaapi = append(vaapi, node)
		}
	}
	return nvidia, vaapi
}

func readSysfsHex(path string) (uint64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	value, err := strconv.ParseUint(strings.TrimSpace(string(data)), 0, 32)
	return value, err == nil
}
