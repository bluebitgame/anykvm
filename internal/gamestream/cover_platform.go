package gamestream

import (
	"os"
	"runtime"
	"strings"
)

func ScreenPlatform() string {
	if runtime.GOOS == "linux" {
		return linuxCoverPlatform(os.ReadFile)
	}
	return runtime.GOOS
}

func linuxCoverPlatform(readFile func(string) ([]byte, error)) string {
	// /etc overrides the vendor file, even when its distribution is unknown.
	for _, path := range []string{"/etc/os-release", "/usr/lib/os-release"} {
		data, err := readFile(path)
		if err != nil {
			continue
		}
		fields := make(map[string]string)
		for _, line := range strings.Split(string(data), "\n") {
			key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
			if !ok {
				continue
			}
			value = strings.TrimSpace(value)
			if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
				value = value[1 : len(value)-1]
			}
			fields[strings.TrimSpace(key)] = value
		}
		id := strings.ToLower(fields["ID"])
		// Raspberry Pi OS 64-bit retains Debian's ID. Match the image marker,
		// rather than the hardware, so Debian running on a Pi stays Debian.
		if id == "debian" {
			if marker, err := readFile("/etc/rpi-issue"); err == nil && strings.HasPrefix(strings.TrimSpace(string(marker)), "Raspberry Pi reference ") {
				return "linux-raspbian"
			}
		}
		// Use explicit variant identity, not the desktop currently running. Ubuntu
		// flavors sometimes share ID=ubuntu and cannot always be distinguished.
		if variant := strings.ToLower(fields["VARIANT_ID"]); variant != "" && (id == "fedora" || id == "ubuntu") {
			if platform := MachineCoverPlatform("linux-" + id + "-" + variant); platform != "linux" {
				return platform
			}
		}
		if id == "linuxmint" && fields["NAME"] == "LMDE" {
			id = "lmde"
		}
		if (id == "debian" || id == "ubuntu") && strings.HasPrefix(fields["PRETTY_NAME"], "Armbian") {
			id = "armbian"
		}
		// Established ID spellings can share the same artwork. ID_LIKE is never
		// used to identify a derivative as its parent distribution.
		switch id {
		case "aoscos":
			id = "aosc"
		case "archlinux":
			id = "arch"
		case "arco":
			id = "arcolinux"
		case "mxlinux":
			id = "mx"
		case "kdeneon":
			id = "neon"
		case "vanillaos":
			id = "vanilla"
		case "tuxedoos":
			id = "tuxedo"
		case "opensuse-tumbleweed-slowroll":
			id = "opensuse-slowroll"
		}
		return MachineCoverPlatform("linux-" + id)
	}
	return "linux"
}
