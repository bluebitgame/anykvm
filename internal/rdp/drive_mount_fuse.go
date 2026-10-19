//go:build linux || darwin

package rdp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// A session bus also exists for headless systemd users, so DBus presence alone
// is not evidence of a desktop. SSH/X11 forwarding is not a local desktop.
func linuxDesktopSession(getenv func(string) string) bool {
	kind := strings.ToLower(getenv("XDG_SESSION_TYPE"))
	if kind == "tty" || kind == "unspecified" {
		return false
	}
	if kind == "wayland" || kind == "x11" {
		return true
	}
	if getenv("SSH_CONNECTION") != "" || getenv("SSH_TTY") != "" {
		return false
	}
	return getenv("WAYLAND_DISPLAY") != "" || strings.HasPrefix(getenv("DISPLAY"), ":")
}

func linuxDriveMountRoot(getenv func(string) string) string {
	if !linuxDesktopSession(getenv) {
		return "/mnt"
	}
	if root := getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(root) {
		return filepath.Join(root, "anykvm", "drives")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("anykvm-%d", os.Geteuid()), "drives")
}

type linuxMountEntry struct{ id, kind, source string }

func linuxMountTable(r io.Reader) (map[string]linuxMountEntry, error) {
	mounts := make(map[string]linuxMountEntry)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 {
			continue
		}
		sep := 6
		for sep < len(fields) && fields[sep] != "-" {
			sep++
		}
		if sep+3 >= len(fields) {
			continue
		}
		mounts[unescape.Replace(fields[4])] = linuxMountEntry{fields[0], fields[sep+1], unescape.Replace(fields[sep+2])}
	}
	return mounts, scanner.Err()
}
func readLinuxMounts() (map[string]linuxMountEntry, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return linuxMountTable(f)
}

// Claim a new directory atomically; never adopt an existing folder or mount.
func linuxMountDirectory(root, name string) (string, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", fmt.Errorf("prepare mount root %s: %w", root, err)
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, driveVolumeName(name))
	if err := os.Mkdir(path, 0700); err == nil {
		return path, nil
	} else if !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("create mountpoint under %s (directory write permission is required): %w", root, err)
	}
	return os.MkdirTemp(root, driveVolumeName(name)+"-")
}

// Ignore user rclone profiles and proxy variables: this endpoint is private,
// password-free loopback, and must never inherit unrelated remote credentials.
func linuxMountEnvironment(env []string) []string {
	clean := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "RCLONE_") || upper == "HTTP_PROXY" || upper == "HTTPS_PROXY" || upper == "ALL_PROXY" || upper == "NO_PROXY" {
			continue
		}
		clean = append(clean, entry)
	}
	return append(clean, "NO_PROXY=127.0.0.1", "no_proxy=127.0.0.1")
}

func linuxMountArguments(endpoint, path, name string, readOnly bool) []string {
	args := []string{"mount", ":webdav:", path, "--config", "/dev/null", "--webdav-url", endpoint, "--webdav-vendor", "other", "--devname", name, "--vfs-cache-mode", "off", "--dir-cache-time", "1s", "--poll-interval", "0", "--no-modtime", "--umask", "077", "--contimeout", "5s", "--timeout", "45s", "--retries", "1", "--low-level-retries", "1"}
	if readOnly {
		args = append(args, "--read-only")
	}
	return args
}

type mountOutput struct {
	mu   sync.Mutex
	text []byte
}

func (b *mountOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	const limit = 4096
	b.text = append(b.text, p...)
	if len(b.text) > limit {
		b.text = append([]byte(nil), b.text[len(b.text)-limit:]...)
	}
	return len(p), nil
}
func (b *mountOutput) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.text))
}

type linuxDriveVolume struct {
	path, name, unmount, id string
	cmd                     *exec.Cmd
	done                    chan struct{}
	waitErr                 error // Read only after done closes.
	output                  mountOutput
	closeMu                 sync.Mutex
}

func (v *linuxDriveVolume) Path() string { return v.path }
func (v *linuxDriveVolume) stop() {
	select {
	case <-v.done:
		return
	default:
	}
	_ = v.cmd.Process.Signal(os.Interrupt)
	select {
	case <-v.done:
		return
	case <-time.After(2 * time.Second):
	}
	_ = v.cmd.Process.Kill()
	<-v.done
}
func (v *linuxDriveVolume) Close() error {
	v.closeMu.Lock()
	defer v.closeMu.Unlock()
	defer v.stop()
	mounts, err := readLinuxMounts()
	if err != nil {
		return err
	}
	entry, mounted := mounts[v.path]
	// A manual replacement must not be unmounted, even at our old pathname.
	if mounted && (entry.kind != "fuse.rclone" || entry.source != v.name || (v.id != "" && entry.id != v.id)) {
		// Do not send a graceful unmount signal: rclone could act on the
		// pathname now occupied by the replacement. Close only our process.
		_ = v.cmd.Process.Kill()
		<-v.done
		return fmt.Errorf("mountpoint %s belongs to another mount; left untouched", v.path)
	}
	if mounted {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		out, err := exec.CommandContext(ctx, v.unmount, "-uz", v.path).CombinedOutput()
		cancel()
		if err != nil {
			return fmt.Errorf("unmount %s: %w (%s)", v.path, err, strings.TrimSpace(string(out)))
		}
	}
	v.stop()
	if err := os.Remove(v.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func mountLinuxDrive(ctx context.Context, endpoint, name string, readOnly bool, root string) (driveVolume, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	binary, err := exec.LookPath("rclone")
	if err != nil {
		return nil, errors.New("Linux drive auto-mount requires rclone and FUSE (install rclone and fuse3)")
	}
	unmount, err := exec.LookPath("fusermount3")
	if err != nil {
		unmount, err = exec.LookPath("fusermount")
	}
	if err != nil {
		return nil, errors.New("Linux drive auto-mount requires fusermount3 or fusermount")
	}
	path, err := linuxMountDirectory(root, name)
	if err != nil {
		return nil, err
	}
	name = driveVolumeName(name)
	v := &linuxDriveVolume{path: path, name: name, unmount: unmount, done: make(chan struct{})}
	v.cmd = exec.Command(binary, linuxMountArguments(endpoint, path, name, readOnly)...)
	v.cmd.Env = linuxMountEnvironment(os.Environ())
	v.cmd.Stdout = &v.output
	v.cmd.Stderr = &v.output
	v.cmd.WaitDelay = time.Second
	if err := v.cmd.Start(); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	go func() { v.waitErr = v.cmd.Wait(); close(v.done) }()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		mounts, err := readLinuxMounts()
		if err != nil {
			return nil, errors.Join(err, v.Close())
		}
		if entry, ok := mounts[path]; ok && entry.kind == "fuse.rclone" && entry.source == name {
			v.id = entry.id
			if ctx.Err() != nil {
				return nil, errors.Join(ctx.Err(), v.Close())
			}
			return v, nil
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), v.Close())
		case <-v.done:
			// The mount helper must stay alive for the lifetime of this volume.
			err := v.waitErr
			if err == nil {
				err = errors.New("rclone exited before mounting")
			}
			return nil, errors.Join(fmt.Errorf("rclone mount: %w (%s)", err, v.output.String()), v.Close())
		case <-ticker.C:
		}
	}
}
