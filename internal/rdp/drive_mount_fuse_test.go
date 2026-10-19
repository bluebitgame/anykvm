//go:build linux || darwin

package rdp

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDriveLinuxDesktopDetection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     map[string]string
		desktop bool
	}{
		{"headless", nil, false},
		{"session bus alone", map[string]string{"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus"}, false},
		{"SSH X11 forwarding", map[string]string{"SSH_CONNECTION": "remote", "DISPLAY": "localhost:10.0"}, false},
		{"SSH tty with inherited desktop", map[string]string{"XDG_SESSION_TYPE": "tty", "DISPLAY": ":0"}, false},
		{"GNOME Wayland", map[string]string{"XDG_SESSION_TYPE": "wayland", "XDG_CURRENT_DESKTOP": "GNOME"}, true},
		{"KDE X11", map[string]string{"XDG_SESSION_TYPE": "x11", "XDG_CURRENT_DESKTOP": "KDE"}, true},
		{"Wayland socket env", map[string]string{"WAYLAND_DISPLAY": "wayland-0"}, true},
		{"local X11", map[string]string{"DISPLAY": ":0"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			get := func(key string) string { return tc.env[key] }
			if got := linuxDesktopSession(get); got != tc.desktop {
				t.Fatalf("desktop=%v", got)
			}
			root := linuxDriveMountRoot(get)
			if !tc.desktop && root != "/mnt" {
				t.Fatalf("headless mount root=%s", root)
			}
		})
	}
	env := map[string]string{"XDG_SESSION_TYPE": "wayland", "XDG_RUNTIME_DIR": "/run/user/1000"}
	if got := linuxDriveMountRoot(func(key string) string { return env[key] }); got != "/run/user/1000/anykvm/drives" {
		t.Fatal(got)
	}
}

func TestDriveLinuxMountDirectoryIsolation(t *testing.T) {
	root := t.TempDir()
	// macOS's temporary directory may be reached through /var -> /private/var.
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	first, err := linuxMountDirectory(root, "CLIENT")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first, "existing"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := linuxMountDirectory(root, "CLIENT")
	if err != nil {
		t.Fatal(err)
	}
	if second == first || !strings.HasPrefix(filepath.Base(second), "CLIENT-") {
		t.Fatal("existing directory reused", second)
	}
	third, err := linuxMountDirectory(root, "../../outside")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(third) != root {
		t.Fatal("client name escaped mount root")
	}
	if data, err := os.ReadFile(filepath.Join(first, "existing")); err != nil || string(data) != "keep" {
		t.Fatal("existing files changed")
	}
}

func TestDriveLinuxMountTable(t *testing.T) {
	table, err := linuxMountTable(strings.NewReader("31 22 0:3 / /run/user/1000/A\\040B rw,nosuid - fuse.rclone CLIENT\\040PC rw,user_id=1000\n32 22 0:4 / /mnt/manual rw - ext4 /dev/sdb1 rw\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := table["/run/user/1000/A B"]; got.id != "31" || got.kind != "fuse.rclone" || got.source != "CLIENT PC" {
		t.Fatal(got)
	}
	if table["/mnt/manual"].kind != "ext4" {
		t.Fatal("manual volume missing")
	}
}

func TestDriveLinuxMountEnvironmentAndPermissions(t *testing.T) {
	env := linuxMountEnvironment([]string{"PATH=/bin", "HOME=/home/user", "RCLONE_WEBDAV_PASS=unrelated", "HTTP_PROXY=unrelated", "RCLONE_CONFIG=/unrelated", "no_proxy=old"})
	if !reflect.DeepEqual(env, []string{"PATH=/bin", "HOME=/home/user", "NO_PROXY=127.0.0.1", "no_proxy=127.0.0.1"}) {
		t.Fatal("inherited remote settings", env)
	}
	for _, ro := range []bool{false, true} {
		args := linuxMountArguments("http://127.0.0.1:12345/", "/mnt/CLIENT", "CLIENT", ro)
		flags := strings.Join(args, " ")
		if strings.Contains(flags, "--read-only") != ro || !strings.Contains(flags, "--vfs-cache-mode off") || !strings.Contains(flags, "--config /dev/null") {
			t.Fatal(flags)
		}
	}
}
