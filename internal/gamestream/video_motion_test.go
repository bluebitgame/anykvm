//go:build cgo

package gamestream

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
)

// Exercise changing source frames and P-frame decoding; a first-IDR-only test
// cannot detect a stream which displays one frame then freezes.
func TestVideoMotionAcrossFrames(t *testing.T) {
	ff := os.Getenv("ANYKVM_TEST_FFMPEG")
	if ff == "" {
		t.Skip("set ANYKVM_TEST_FFMPEG")
	}
	const packetSize = 1024
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	socket, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	makeFrame := func(red bool) bridge.Frame {
		p := make([]byte, 320*240*4)
		for i := 0; i < len(p); i += 4 {
			p[i+3] = 255
			if red {
				p[i+2] = 255
			} else {
				p[i+1] = 255
			}
		}
		return bridge.Frame{Format: capture.FormatRaw, Width: 320, Height: 240, Data: p}
	}
	x := &streamSession{
		server: &Server{videoSocket: socket}, ctx: ctx,
		options: streamOptions{
			Width: 320, Height: 240, FPS: 30, Bitrate: 2000,
			PacketSize: packetSize, MinFEC: 2,
		},
		videoPeer: peer.LocalAddr().(*net.UDPAddr),
		frames:    make(chan bridge.Frame, 1), keyframe: make(chan struct{}, 1),
	}
	done := make(chan error, 1)
	go func() { _, err := x.encodeVideo(makeFrame(true), &videoPacketizer{}, time.Now()); done <- err }()
	defer func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	}()
	peer.SetReadDeadline(time.Now().Add(8 * time.Second))
	var stream, frame []byte
	frames := 0
	var fid uint32
	buf := make([]byte, 2048)
	for frames < 40 {
		n, _, err := peer.ReadFromUDP(buf)
		if err != nil {
			t.Fatal(err)
		}
		p := buf[:n]
		if n < 32 {
			t.Fatal("short RTP")
		}
		info := binary.LittleEndian.Uint32(p[28:])
		if int(info>>12&1023) >= int(info>>22) {
			continue
		}
		if p[24]&4 != 0 {
			if len(frame) != 0 {
				t.Fatal("unfinished frame")
			}
			fid = binary.LittleEndian.Uint32(p[20:])
		}
		if binary.LittleEndian.Uint32(p[20:]) != fid {
			t.Fatal("mixed frames")
		}
		frame = append(frame, p[32:]...)
		if p[24]&2 != 0 {
			last := int(binary.LittleEndian.Uint16(frame[4:]))
			frame = frame[:len(frame)-(packetSize-16)+last]
			stream = append(stream, frame[8:]...)
			frame = nil
			frames++
			if frames == 10 {
				x.frames <- makeFrame(false)
			}
		}
	}
	decode := exec.CommandContext(ctx, ff, "-hide_banner", "-loglevel", "error", "-xerror", "-f", "h264", "-i", "pipe:0", "-f", "rawvideo", "-pix_fmt", "rgb24", "pipe:1")
	decode.Stdin = bytes.NewReader(stream)
	var stderr bytes.Buffer
	decode.Stderr = &stderr
	raw, err := decode.Output()
	if err != nil {
		t.Fatalf("decode: %v %s", err, stderr.String())
	}
	size := 320 * 240 * 3
	if len(raw) != frames*size {
		t.Fatalf("decoded %d frames, sent %d", len(raw)/size, frames)
	}
	first, last := raw[:3], raw[len(raw)-size:len(raw)-size+3]
	if first[0] < 200 || first[1] > 40 || last[1] < 200 || last[0] > 40 {
		t.Fatalf("motion update lost: first=%v last=%v", first, last)
	}
}
