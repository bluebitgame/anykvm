package scrcpy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveServerUsesExecutableDir(t *testing.T) {
	exeDir := t.TempDir()
	workDir := t.TempDir()
	const name = "scrcpy-server-v3.3.4"
	for _, dir := range []string{exeDir, workDir} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("jar"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(workDir)
	t.Setenv("SCRCPY_SERVER_VERSION", "")
	for _, tc := range []struct {
		name string
		opts Options
		env  string
	}{
		{name: "automatic"},
		{name: "relative config", opts: Options{ServerJar: name}},
		{name: "relative environment", env: name},
		{name: "config overrides environment", opts: Options{ServerJar: name}, env: "missing.jar"},
		{name: "absolute override", opts: Options{ServerJar: filepath.Join(exeDir, name)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SCRCPY_SERVER_PATH", tc.env)
			jar, version, err := resolveServerInDir(tc.opts, exeDir)
			if err != nil || jar != filepath.Join(exeDir, name) || version != "3.3.4" {
				t.Fatalf("resolved (%q, %q, %v); want jar beside executable and version 3.3.4", jar, version, err)
			}
		})
	}
}

func TestResolveServerDoesNotUseWorkingDir(t *testing.T) {
	exeDir := t.TempDir()
	workDir := t.TempDir()
	const name = "scrcpy-server-v3.3.4"
	if err := os.WriteFile(filepath.Join(workDir, name), []byte("jar"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workDir)
	t.Setenv("SCRCPY_SERVER_PATH", "")
	for _, opts := range []Options{{}, {ServerJar: name}} {
		if _, _, err := resolveServerInDir(opts, exeDir); err == nil || !strings.Contains(err.Error(), exeDir) {
			t.Fatalf("expected missing executable-side jar with its directory in the error, got %v", err)
		}
	}
}

func TestServerJarInDir(t *testing.T) {
	dir := t.TempDir()
	touch := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("jar"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	if got := serverJarInDir(dir); got != "" {
		t.Fatalf("empty dir: got %q", got)
	}

	touch("scrcpy-server-v3.1")
	touch("scrcpy-server-v3.3.4")
	touch("scrcpy-server-v2.7")
	if got := serverJarInDir(dir); filepath.Base(got) != "scrcpy-server-v3.3.4" {
		t.Fatalf("versioned: got %q, want scrcpy-server-v3.3.4", got)
	}

	// The exact name always wins over versioned downloads.
	touch("scrcpy-server")
	if got := serverJarInDir(dir); filepath.Base(got) != "scrcpy-server" {
		t.Fatalf("exact: got %q, want scrcpy-server", got)
	}
}

func TestVersionFromPath(t *testing.T) {
	cases := map[string]string{
		"/apps/anykvm/scrcpy-server-v3.3.4":                          "3.3.4",
		"/apps/anykvm/scrcpy-server-v3.1":                            "3.1",
		"/opt/homebrew/Cellar/scrcpy/3.1/share/scrcpy/scrcpy-server": "3.1",
		"/usr/share/scrcpy/scrcpy-server":                            "",
	}
	for path, want := range cases {
		if got := versionFromPath(path); got != want {
			t.Errorf("versionFromPath(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestVersionLess(t *testing.T) {
	if !versionLess(parseVersionParts("3.1"), parseVersionParts("3.3.4")) {
		t.Error("3.1 should be less than 3.3.4")
	}
	if versionLess(parseVersionParts("10.0"), parseVersionParts("9.9")) {
		t.Error("10.0 should not be less than 9.9")
	}
	if versionLess(parseVersionParts("3.3"), parseVersionParts("3.3")) {
		t.Error("equal versions should not be less")
	}
}
