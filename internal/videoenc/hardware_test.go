package videoenc

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLinuxH264HardwareVendors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		vendors    []string
		nvidia     bool
		vaapiCount int
	}{
		{"AMD", []string{"0x1002"}, false, 1},
		{"Intel", []string{"0x8086"}, false, 1},
		{"NVIDIA", []string{"0x10de"}, true, 0},
		{"mixed", []string{"0x10de", "0x1002", "0x8086"}, true, 2},
		{"unknown", []string{"0x1234", "invalid", ""}, false, 0},
		{"none", nil, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			sys, dev := filepath.Join(root, "sys"), filepath.Join(root, "dev/dri")
			for i, vendor := range tc.vendors {
				node := "renderD" + string(rune('0'+i))
				writeHardwareFixture(t, filepath.Join(dev, node), "")
				if vendor != "" {
					writeHardwareFixture(t, filepath.Join(sys, "class/drm", node, "device/vendor"), vendor+"\n")
				}
			}
			nvidia, nodes := linuxH264Hardware(sys, dev)
			if nvidia != tc.nvidia || len(nodes) != tc.vaapiCount {
				t.Fatalf("nvidia=%v vaapi=%v", nvidia, nodes)
			}

		})
	}
}

func TestLinuxH264NvidiaWithoutDRM(t *testing.T) {
	for _, tc := range []struct {
		class string
		want  bool
	}{
		{"0x030000", true}, {"0x030200", true}, {"0x040300", false}, {"broken", false},
	} {
		t.Run(tc.class, func(t *testing.T) {
			root := t.TempDir()
			writeHardwareFixture(t, filepath.Join(root, "bus/pci/devices/0000:01:00.0/vendor"), "0x10de\n")
			writeHardwareFixture(t, filepath.Join(root, "bus/pci/devices/0000:01:00.0/class"), tc.class+"\n")
			nvidia, nodes := linuxH264Hardware(root, filepath.Join(root, "no-devices"))
			if nvidia != tc.want || len(nodes) != 0 {
				t.Fatalf("nvidia=%v vaapi=%v", nvidia, nodes)
			}
		})
	}
}

func writeHardwareFixture(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
}
