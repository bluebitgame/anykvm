package scrcpy

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// adbBinaryName is the adb executable name for the current platform.
func adbBinaryName() string {
	if runtime.GOOS == "windows" {
		return "adb.exe"
	}
	return "adb"
}

// findADB locates the adb binary. Like the ffmpeg resolver, a copy bundled
// next to the AnyKVM executable (in platform-tools/) is preferred, so a
// bundled adb works regardless of PATH. Order: $ADB (explicit override),
// <exe dir>/platform-tools/adb, PATH, then common Android SDK paths.
func findADB() (string, error) {
	if p := os.Getenv("ADB"); p != "" {
		if isExecutableFile(p) {
			return p, nil
		}
	}
	if exe, err := os.Executable(); err == nil {
		if resolved, err2 := filepath.EvalSymlinks(exe); err2 == nil {
			exe = resolved
		}
		cand := filepath.Join(filepath.Dir(exe), "platform-tools", adbBinaryName())
		if isExecutableFile(cand) {
			return cand, nil
		}
	}
	if p, err := exec.LookPath(adbBinaryName()); err == nil {
		return p, nil
	}
	var cands []string
	for _, env := range []string{"ANDROID_HOME", "ANDROID_SDK_ROOT"} {
		if sdk := os.Getenv(env); sdk != "" {
			cands = append(cands, filepath.Join(sdk, "platform-tools", adbBinaryName()))
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		cands = append(cands,
			filepath.Join(home, "Library", "Android", "sdk", "platform-tools", adbBinaryName()), // macOS
			filepath.Join(home, "Android", "Sdk", "platform-tools", adbBinaryName()),            // Linux
		)
	}
	for _, c := range cands {
		if isExecutableFile(c) {
			return c, nil
		}
	}
	return "", fmt.Errorf("scrcpy: adb not found (bundle platform-tools next to the executable, install Android platform-tools, or set $ADB)")
}

// resolveServer locates the scrcpy-server jar beside the executable and
// determines its version. Relative overrides use the executable directory,
// never the process working directory.
// The version string must exactly match the jar's build version: the server
// refuses to start otherwise.
func resolveServer(opts Options) (jar, version string, err error) {
	exe, err := os.Executable()
	if err != nil {
		return "", "", fmt.Errorf("scrcpy: locate executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return resolveServerInDir(opts, filepath.Dir(exe))
}

func resolveServerInDir(opts Options, exeDir string) (jar, version string, err error) {
	jar = opts.ServerJar
	if jar == "" {
		jar = os.Getenv("SCRCPY_SERVER_PATH")
	}
	if jar == "" {
		jar = serverJarInDir(exeDir)
	} else if !filepath.IsAbs(jar) {
		jar = filepath.Join(exeDir, jar)
	}
	if jar == "" || !fileExists(jar) {
		return "", "", fmt.Errorf("scrcpy: scrcpy-server not found (executable directory: %s, requested path: %q; bundle scrcpy-server beside the executable or set scrcpy.server / $SCRCPY_SERVER_PATH)", exeDir, jar)
	}

	version = opts.Version
	if version == "" {
		version = os.Getenv("SCRCPY_SERVER_VERSION")
	}
	if version == "" {
		version = versionFromPath(jar)
	}
	if version == "" {
		version = versionFromBinary()
	}
	if version == "" {
		return "", "", fmt.Errorf("scrcpy: cannot determine scrcpy-server version for %s (set scrcpy.version / $SCRCPY_SERVER_VERSION)", jar)
	}
	return jar, version, nil
}

// serverJarInDir returns a scrcpy-server jar in dir: the exact name
// "scrcpy-server", or a versioned "scrcpy-server-vX.Y[.Z]" (as published on
// GitHub releases; the highest version wins when several are present).
func serverJarInDir(dir string) string {
	if p := filepath.Join(dir, "scrcpy-server"); fileExists(p) {
		return p
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "scrcpy-server-*"))
	best := ""
	var bestVer []int
	for _, m := range matches {
		sm := jarFileVersionRe.FindStringSubmatch(filepath.Base(m))
		if sm == nil || !fileExists(m) {
			continue
		}
		v := parseVersionParts(sm[1])
		if best == "" || versionLess(bestVer, v) {
			best, bestVer = m, v
		}
	}
	return best
}

// parseVersionParts splits "3.1" / "3.3.4" into integer parts.
func parseVersionParts(s string) []int {
	fields := strings.Split(s, ".")
	parts := make([]int, len(fields))
	for i, f := range fields {
		parts[i], _ = strconv.Atoi(f)
	}
	return parts
}

// versionLess reports whether version a is lower than b.
func versionLess(a, b []int) bool {
	for i := 0; i < len(a) || i < len(b); i++ {
		var av, bv int
		if i < len(a) {
			av = a[i]
		}
		if i < len(b) {
			bv = b[i]
		}
		if av != bv {
			return av < bv
		}
	}
	return false
}

var (
	pathVersionRe    = regexp.MustCompile(`(?i)/scrcpy[/@-]v?(\d+\.\d+(?:\.\d+)?)(?:[/_]|$)`)
	jarFileVersionRe = regexp.MustCompile(`(?i)^scrcpy-server-v?(\d+\.\d+(?:\.\d+)?)$`)
)

// versionFromPath extracts the scrcpy version from the jar's filename (e.g.
// scrcpy-server-v3.1) or its (resolved) path, e.g.
// /opt/homebrew/Cellar/scrcpy/3.1/share/scrcpy/scrcpy-server -> "3.1".
func versionFromPath(jar string) string {
	if m := jarFileVersionRe.FindStringSubmatch(filepath.Base(jar)); m != nil {
		return m[1]
	}
	if resolved, err := filepath.EvalSymlinks(jar); err == nil {
		jar = resolved
	}
	if m := pathVersionRe.FindStringSubmatch(filepath.ToSlash(jar)); m != nil {
		return m[1]
	}
	return ""
}

// versionFromBinary parses "scrcpy X.Y" from `scrcpy --version`.
func versionFromBinary() string {
	out, err := exec.Command("scrcpy", "--version").Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(strings.SplitN(string(out), "\n", 2)[0])
	if len(fields) >= 2 && fields[0] == "scrcpy" {
		return fields[1]
	}
	return ""
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// isExecutableFile reports whether path is a regular file that is executable.
func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true // executability is determined by extension on Windows
	}
	return info.Mode()&0111 != 0
}
