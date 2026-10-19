//go:build cgo

package gamestream

import (
	"anykvm/internal/bridge"
	"anykvm/internal/capture"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestAV1DirectStream(t *testing.T) {
	data, err := os.ReadFile("../testdata/stream.ivf")
	if err != nil {
		t.Fatal(err)
	}
	data = data[32:]
	var units [][]byte
	for len(data) > 0 {
		n := int(binary.LittleEndian.Uint32(data))
		units = append(units, data[12:12+n])
		data = data[12+n:]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sender, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	receiver, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	x := &streamSession{ctx: ctx, server: &Server{videoSocket: sender}, videoPeer: receiver.LocalAddr().(*net.UDPAddr), frames: make(chan bridge.Frame, 6), keyframe: make(chan struct{}, 1), options: streamOptions{Codec: "av1", Width: 64, Height: 48, FPS: 30, PacketSize: 1024, MinFEC: 2}}
	epoch := time.Now()
	frame := func(i int) bridge.Frame {
		return bridge.Frame{Data: units[i], Format: capture.FormatAV1, Width: 64, Height: 48, CapturedAt: epoch.Add(time.Duration(i) * time.Second / 30)}
	}
	done := make(chan error, 1)
	go func() { _, err := x.streamEncodedVideo(frame(0), &videoPacketizer{}, epoch); done <- err }()
	defer func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	}()
	receiver.SetReadDeadline(time.Now().Add(4 * time.Second))
	var stream []byte
	for i := 0; i < 6; i++ {
		if i > 0 {
			x.frames <- frame(i)
		}
		var assembled []byte
		for {
			packet := make([]byte, 2048)
			n, _, err := receiver.ReadFromUDP(packet)
			if err != nil {
				t.Fatal(err)
			}
			packet = packet[:n]
			fec := binary.LittleEndian.Uint32(packet[28:])
			if fec>>12&1023 >= fec>>22 {
				continue
			}
			if binary.LittleEndian.Uint32(packet[20:]) != uint32(i+1) {
				t.Fatal("unexpected frame index")
			}
			if got := binary.BigEndian.Uint32(packet[4:]); got != captureRTPTimestamp(frame(i).CapturedAt, epoch, epoch) {
				t.Fatalf("timestamp %d", got)
			}
			assembled = append(assembled, packet[32:]...)
			if packet[24]&2 != 0 {
				break
			}
		}
		tail := int(binary.LittleEndian.Uint16(assembled[4:]))
		assembled = assembled[:len(assembled)-(1024-16)+tail]
		if (assembled[3] == 2) != (i%3 == 0) {
			t.Fatal("wrong recovery flag")
		}
		if !bytes.Equal(assembled[8:], units[i]) {
			t.Fatal("OBU payload changed")
		}
		stream = append(stream, assembled[8:]...)
	}
	if ff := os.Getenv("ANYKVM_TEST_FFMPEG"); ff != "" {
		cmd := exec.CommandContext(ctx, ff, "-v", "error", "-f", "obu", "-i", "pipe:0", "-pix_fmt", "rgb24", "-f", "rawvideo", "pipe:1")
		cmd.Stdin = bytes.NewReader(stream)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 6*64*48*3 || bytes.Equal(out[:64*48*3], out[64*48*3:2*64*48*3]) {
			t.Fatalf("decoded frames missing or frozen: %d bytes", len(out))
		}
	}
}
