//go:build cgo

package gamestream

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"anykvm/internal/capture"
)

// A native VideoToolbox fixture consists of big-endian lengths followed by
// complete Annex-B access units. The benchmark at
// tools/codec-bench/macos/videotoolbox_encode_bench.m can produce it without
// screen capture. Decode after actual GameStream packetization and reassembly.
func TestNativeHEVCPacketDecode(t *testing.T) {
	path, ff := os.Getenv("ANYKVM_TEST_HEVC_FRAMES"), os.Getenv("ANYKVM_TEST_FFMPEG")
	if path == "" || ff == "" {
		t.Skip("set ANYKVM_TEST_HEVC_FRAMES and ANYKVM_TEST_FFMPEG")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	packetizer := &videoPacketizer{}
	var bitstream bytes.Buffer
	frames := 0
	for {
		var length uint32
		if err := binary.Read(f, binary.BigEndian, &length); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if length == 0 || length > 8*1024*1024 {
			t.Fatal("invalid fixture AU size")
		}
		au := make([]byte, length)
		if _, err := io.ReadFull(f, au); err != nil {
			t.Fatal(err)
		}
		key := isRecoveryFrame(au, capture.FormatHEVC)
		if frames == 0 && !key {
			t.Fatal("missing initial HEVC recovery frame")
		}
		packets, err := packetizer.pack(au, key, uint32(frames*3000), 1024, 2)
		if err != nil {
			t.Fatal(err)
		}
		var recovered []byte
		for _, p := range packets {
			info := binary.LittleEndian.Uint32(p[28:])
			if info>>12&1023 >= info>>22 {
				continue
			}
			recovered = append(recovered, p[32:]...)
		}
		if !bytes.Equal(recovered[8:8+length], au) {
			t.Fatal("HEVC packet payload changed")
		}
		bitstream.Write(recovered[8 : 8+length])
		frames++
	}
	if frames < 60 {
		t.Fatal("fixture too short for continuous decoding")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ff, "-hide_banner", "-loglevel", "error", "-xerror", "-f", "hevc", "-i", "pipe:0", "-f", "framemd5", "pipe:1")
	cmd.Stdin = &bitstream
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("HEVC decode: %v %s", err, &stderr)
	}
	decoded := 0
	unique := map[string]bool{}
	for _, line := range bytes.Split(out, []byte{'\n'}) {
		if len(line) > 0 && line[0] != '#' {
			decoded++
			parts := bytes.Split(line, []byte{','})
			unique[string(bytes.TrimSpace(parts[len(parts)-1]))] = true
		}
	}
	if decoded != frames || len(unique) < 2 {
		t.Fatalf("decoded=%d sent=%d unique=%d", decoded, frames, len(unique))
	}
}
