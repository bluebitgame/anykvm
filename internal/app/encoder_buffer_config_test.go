package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRDPBufferFramesConfig(t *testing.T) {
	for _, frames := range []int{-1, 0, 1, 3, 8, 9} {
		t.Run(fmt.Sprint(frames), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			source := fmt.Sprintf("rdp: {buffer_frames: %d}\n", frames)
			if err := os.WriteFile(path, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if frames < 0 || frames > 8 {
				if err == nil || !strings.Contains(err.Error(), "rdp.buffer_frames") {
					t.Fatalf("validation: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.RDP.BufferFrames != frames {
				t.Fatal("RDP encoder limit lost")
			}
			if err := SaveConfig(path, cfg); err != nil {
				t.Fatal(err)
			}
			again, err := LoadConfig(path)
			if err != nil || again.RDP != cfg.RDP {
				t.Fatalf("round trip: %v", err)
			}
		})
	}
}
