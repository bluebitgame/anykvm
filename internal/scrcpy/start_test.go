package scrcpy

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Exercise Start through a fake adb and actual TCP sockets. If audio/control
// order or metadata reads regress, this handshake fails instead of deadlocking.
func TestStartVideoAudioControl(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake adb uses a POSIX shell")
	}
	for _, tc := range []struct {
		version, codec string
		mode           string
		audio          bool
		audioCode      uint32
	}{
		{"3.3.4", "h265", "", true, codecIDRaw},
		{"5.0.1", "h265", "", true, 0},
		{"5.0.1", "h264", "auto@30", false, 0},
		{"3.0", "h264", "1280x720@30", false, 0},
		{"5.0.1", "h265", "1280x720@30", true, codecIDRaw},
	} {
		t.Run(tc.version+tc.codec+fmt.Sprint(tc.audio), func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			port := listener.Addr().(*net.TCPAddr).Port
			dir := t.TempDir()
			adb := filepath.Join(dir, "adb")
			script := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in\npush) exit 0;;\nforward) if [ \"$2\" != \"--remove\" ]; then echo %d; fi;;\nshell) exec /bin/sleep 60;;\nesac\n", port)
			if err := os.WriteFile(adb, []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			jar := filepath.Join(dir, "scrcpy-server-v"+tc.version)
			if err := os.WriteFile(jar, []byte("test"), 0644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("ADB", adb)
			t.Setenv("SCRCPY_SERVER_VERSION", "")
			result := make(chan error, 1)
			go func() {
				result <- func() error {
					accept := func() (net.Conn, error) {
						conn, err := listener.Accept()
						if err == nil {
							conn.SetDeadline(time.Now().Add(5 * time.Second))
						}
						return conn, err
					}
					video, err := accept()
					if err != nil {
						return err
					}
					defer video.Close()
					if _, err = video.Write([]byte{0}); err != nil {
						return err
					}
					var audio net.Conn
					if tc.audio {
						audio, err = accept()
						if err != nil {
							return err
						}
						defer audio.Close()
					}
					control, err := accept()
					if err != nil {
						return err
					}
					defer control.Close()
					var header bytes.Buffer
					codec := uint32(codecIDH264)
					if tc.codec == "h265" {
						codec = codecIDH265
					}
					writeWords(&header, codec)
					if !versionLess(parseVersionParts(tc.version), []int{4, 0}) {
						writeWords(&header, 0x80000000)
					}
					writeWords(&header, 1280, 720)
					if _, err = video.Write(header.Bytes()); err != nil {
						return err
					}
					if audio != nil {
						if err = binary.Write(audio, binary.BigEndian, tc.audioCode); err != nil {
							return err
						}
					}
					var command [1]byte
					if _, err = io.ReadFull(control, command[:]); err != nil {
						return err
					}
					if command[0] != 17 {
						return fmt.Errorf("wrong socket order: control byte %d", command[0])
					}
					return nil
				}()
			}()
			sess := NewSession(Options{ServerJar: jar, Audio: tc.audio})
			dev := NewCapture(sess)
			defer dev.Close()
			if err := dev.SetCaptureMode(tc.mode); err != nil {
				t.Fatal(err)
			}
			codec := "h264"
			if tc.codec == "h265" {
				codec = "hevc"
			}
			dev.SetVideoCodec(codec)
			if err := dev.OpenWithFormat(1280, 720, 30); err != nil {
				t.Fatal(err)
			}
			if w, h := sess.Size(); w != 1280 || h != 720 || !sess.Running() {
				t.Fatalf("connected session=%dx%d running=%v", w, h, sess.Running())
			}
			if err := dev.RequestKeyframe(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-result:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("server did not complete socket handshake")
			}
		})
	}
}

func TestServerBefore30Rejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake adb uses a POSIX shell")
	}
	jar := filepath.Join(t.TempDir(), "scrcpy-server-v2.7")
	if err := os.WriteFile(jar, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ADB", jar)
	t.Setenv("SCRCPY_SERVER_VERSION", "")
	for _, mode := range []string{"", "1920x1080@30"} {
		sess := NewSession(Options{ServerJar: jar})
		dev := NewCapture(sess)
		if err := dev.SetCaptureMode(mode); err != nil {
			t.Fatal(err)
		}
		if err := sess.Start(0, 0); err == nil || !strings.Contains(err.Error(), ">= 3.0 is required") {
			t.Fatalf("old server must be rejected before launch (mode=%q): %v", mode, err)
		}
	}
}

func TestKeepActiveRequiresNewServer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake adb uses a POSIX shell")
	}
	jar := filepath.Join(t.TempDir(), "scrcpy-server-v3.3.4")
	if err := os.WriteFile(jar, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ADB", jar)
	t.Setenv("SCRCPY_SERVER_VERSION", "")
	sess := NewSession(Options{ServerJar: jar, KeepActive: true})
	if err := sess.Start(0, 0); err == nil || !strings.Contains(err.Error(), "keep_active requires") {
		t.Fatalf("old server must reject keep_active before launch: %v", err)
	}
}
