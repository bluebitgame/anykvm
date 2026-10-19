package gamestream

import (
	"os"
	"testing"
)

func TestLinuxCoverPlatform(t *testing.T) {
	for _, tc := range []struct {
		name, system, vendor, want string
	}{
		{"Debian", "NAME=Debian GNU/Linux\nID=debian\n", "", "linux-debian"},
		{"Arch", "ID=arch\n", "", "linux-arch"},
		{"Fedora quoted", "ID=\"fedora\"\n", "", "linux-fedora"},
		{"Ubuntu quoted", "ID='ubuntu'\nID_LIKE=debian\n", "", "linux-ubuntu"},
		{"whitespace and comments", "# ID=debian\n ID = arch \r\n", "", "linux-arch"},
		{"vendor fallback", "", "ID=ubuntu\n", "linux-ubuntu"},
		{"system precedence", "ID=fedora\n", "ID=debian\n", "linux-fedora"},
		{"unknown system precedence", "ID=example\n", "ID=debian\n", "linux"},
		{"Manjaro", "ID=manjaro\nID_LIKE=arch\n", "", "linux-manjaro"},
		{"Linux Mint", "ID=linuxmint\nID_LIKE=ubuntu\n", "", "linux-linuxmint"},
		{"Pop OS", "ID=pop\nID_LIKE=ubuntu\n", "", "linux-pop"},
		{"EndeavourOS", "ID=endeavouros\nID_LIKE=arch\n", "", "linux-endeavouros"},
		{"openSUSE", "ID=opensuse\n", "", "linux-opensuse"},
		{"openSUSE Leap", "ID=\"opensuse-leap\"\n", "", "linux-opensuse-leap"},
		{"openSUSE Tumbleweed", "ID=\"opensuse-tumbleweed\"\n", "", "linux-opensuse-tumbleweed"},
		{"NixOS", "ID=nixos\n", "", "linux-nixos"},
		{"Kali", "ID=kali\nID_LIKE=debian\n", "", "linux-kali"},
		{"Rocky Linux", "ID=rocky\nID_LIKE=\"rhel centos fedora\"\n", "", "linux-rocky"},
		{"AlmaLinux", "ID=almalinux\nID_LIKE=\"rhel centos fedora\"\n", "", "linux-almalinux"},
		{"Raspbian", "ID=raspbian\nID_LIKE=debian\n", "", "linux-raspbian"},
		{"SteamOS", "ID=steamos\nID_LIKE=arch\n", "", "linux-steamos"},
		{"Bazzite", "ID=bazzite\nID_LIKE=\"fedora\"\n", "", "linux-bazzite"},
		{"CachyOS", "ID=cachyos\nID_LIKE=arch\n", "", "linux-cachyos"},
		{"Garuda", "ID=garuda\nID_LIKE=arch\n", "", "linux-garuda"},
		{"Void", "ID=void\n", "", "linux-void"},
		{"unknown derivative", "ID=example\nID_LIKE=arch\n", "", "linux"},
		{"Fedora Silverblue", "ID=fedora\nVARIANT_ID=silverblue\n", "", "linux-fedora-silverblue"},
		{"Fedora Kinoite", "ID=fedora\nVARIANT_ID=kinoite\n", "", "linux-fedora-kinoite"},
		{"Fedora workstation", "ID=fedora\nVARIANT_ID=workstation\n", "", "linux-fedora"},
		{"Ubuntu MATE identity", "ID=ubuntu\nVARIANT_ID=mate\n", "", "linux-ubuntu-mate"},
		{"Ubuntu desktop does not imply a flavor", "ID=ubuntu\nVARIANT_ID=desktop\n", "", "linux-ubuntu"},
		{"LMDE", "ID=linuxmint\nNAME=LMDE\n", "", "linux-lmde"},
		{"Armbian image", "ID=debian\nPRETTY_NAME=\"Armbian 26.8\"\n", "", "linux-armbian"},
		{"Armbian Ubuntu image", "ID=ubuntu\nPRETTY_NAME=\"Armbian_community 26.8\"\n", "", "linux-armbian"},
		{"AOSC alias", "ID=aoscos\n", "", "linux-aosc"},
		{"Vanilla alias", "ID=vanillaos\n", "", "linux-vanilla"},
		{"TUXEDO alias", "ID=tuxedoos\n", "", "linux-tuxedo"},
		{"Slowroll alias", "ID=opensuse-tumbleweed-slowroll\n", "", "linux-opensuse-slowroll"},
		{"missing ID", "NAME=Ubuntu\nID_LIKE=debian\n", "", "linux"},
		{"malformed ID", "ID=\"ubuntu\n", "", "linux"},
		{"last ID wins", "ID=debian\nID=ubuntu\n", "", "linux-ubuntu"},
		{"no release files", "", "", "linux"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			readFile := func(path string) ([]byte, error) {
				if path != "/etc/os-release" && path != "/usr/lib/os-release" {
					return nil, os.ErrNotExist
				}
				data := tc.system
				if path == "/usr/lib/os-release" {
					data = tc.vendor
				}
				if data == "" {
					return nil, os.ErrNotExist
				}
				return []byte(data), nil
			}
			if got := linuxCoverPlatform(readFile); got != tc.want {
				t.Fatalf("cover = %q, want %q", got, tc.want)
			}
		})
	}
	if got := linuxCoverPlatform(func(string) ([]byte, error) { return nil, os.ErrPermission }); got != "linux" {
		t.Fatalf("unreadable release files: %q", got)
	}
}

func TestLinuxCoverPlatformRaspberryPi(t *testing.T) {
	for _, tc := range []struct{ name, id, marker, want string }{
		{"64-bit image", "debian", "Raspberry Pi reference 2026-10-01\nGenerated using pi-gen\n", "linux-raspbian"},
		{"32-bit image", "raspbian", "", "linux-raspbian"},
		{"ordinary Debian", "debian", "", "linux-debian"},
		{"unrelated marker", "debian", "Custom image\n", "linux-debian"},
		{"Ubuntu on a Pi", "ubuntu", "Raspberry Pi reference 2026-10-01\n", "linux-ubuntu"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := linuxCoverPlatform(func(path string) ([]byte, error) {
				switch path {
				case "/etc/os-release":
					return []byte("ID=" + tc.id + "\n"), nil
				case "/etc/rpi-issue":
					if tc.marker != "" {
						return []byte(tc.marker), nil
					}
				}
				return nil, os.ErrNotExist
			})
			if got != tc.want {
				t.Fatalf("cover = %q, want %q", got, tc.want)
			}
		})
	}
}
