//go:build cgo

package gamestream

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"anykvm/internal/avmedia"
	"anykvm/internal/bridge"
	"anykvm/internal/capture"
)

// Read one complete small fixture picture, excluding FEC parity. Retain the
// actual wire clock and frame index to check encoder transitions.
func receivePassivePicture(t *testing.T, peer *net.UDPConn, packetSize int) ([]byte, uint32, uint32) {
	t.Helper()
	peer.SetReadDeadline(time.Now().Add(3 * time.Second))
	var payload []byte
	var packet [2048]byte
	var pts, index uint32
	for {
		n, _, err := peer.ReadFromUDP(packet[:])
		if err != nil || n < 32 {
			t.Fatalf("UDP picture: %v (%d bytes)", err, n)
		}
		info := binary.LittleEndian.Uint32(packet[28:])
		if info>>12&1023 >= info>>22 {
			continue
		}
		if len(payload) == 0 {
			pts = binary.BigEndian.Uint32(packet[4:])
			index = binary.LittleEndian.Uint32(packet[20:])
		} else if binary.BigEndian.Uint32(packet[4:]) != pts || binary.LittleEndian.Uint32(packet[20:]) != index {
			t.Fatal("picture interrupted before its final packet")
		}
		payload = append(payload, packet[32:n]...)
		if packet[24]&2 != 0 {
			if len(payload) < 8 {
				t.Fatal("missing short frame header")
			}
			last := int(binary.LittleEndian.Uint16(payload[4:]))
			payload = payload[:len(payload)-(packetSize-16)+last]
			return payload[8:], pts, index
		}
	}
}

// Start the viewer without a sender, wait past Moonlight's observed ten-second
// initial-video timeout, then switch to an older portrait source picture. The
// output must remain decodable and its RTP clock must never jump backwards.
func TestPassiveDecodedVideoViewerBeforeSource(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_NATIVE_MEDIA") == "" {
		t.Skip("set ANYKVM_TEST_NATIVE_MEDIA and ANYKVM_FFMPEG_LIBDIR")
	}
	x, peer, cancel, restarts := passiveVideoSession(t)
	x.options.Width, x.options.Height = 1920, 1080
	x.server.cfg.OnVideoPlaceholderAllowed = func() bool { return true }
	done := make(chan struct{})
	go func() { defer close(done); x.videoLoop() }()
	defer func() { cancel(); <-done }()
	decoder, err := avmedia.NewDecoder("h264")
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	start := time.Now()
	var lastPTS, lastIndex uint32
	for time.Since(start) < 10500*time.Millisecond {
		au, pts, index := receivePassivePicture(t, peer, x.options.PacketSize)
		if lastIndex != 0 && (index != lastIndex+1 || int32(pts-lastPTS) <= 0) {
			t.Fatal("waiting video stopped or its clock went backwards")
		}
		if lastIndex == 0 {
			if time.Since(start) >= 3*time.Second || !isRecoveryFrame(au, capture.FormatH264) {
				t.Fatal("waiting video did not deliver a timely initial IDR")
			}
			f, err := decoder.Decode(au)
			if err != nil || f == nil {
				t.Fatalf("waiting video cannot decode: %v", err)
			}
			if f.Width != 1920 || f.Height != 1080 || f.Data[0] > 10 || f.Data[1] > 10 || f.Data[2] > 10 {
				f.Release()
				t.Fatal("waiting video is not the negotiated black picture")
			}
			f.Release()
		}
		lastPTS, lastIndex = pts, index
	}
	base := time.Now().Add(-5 * time.Second)
	motionStart := time.Now()
	var sourcePTS uint32
	for i := 0; i < 60; i++ {
		pixels := make([]byte, 500*1080*4)
		for off := 0; off < len(pixels); off += 4 {
			pixels[off+3] = 255
			pixels[off+1+i%2] = 255 // Green/red BGRA.
		}
		x.frames <- bridge.Frame{PreserveSource: true, Format: capture.FormatRaw,
			Width: 500, Height: 1080, Data: pixels,
			CapturedAt: base.Add(time.Duration(i) * time.Second / 60)}
		for {
			au, pts, index := receivePassivePicture(t, peer, x.options.PacketSize)
			if index != lastIndex+1 || int32(pts-lastPTS) <= 0 {
				t.Fatalf("transition changed frame continuity or clock: index=%d after=%d PTS=%d after=%d", index, lastIndex, pts, lastPTS)
			}
			lastPTS, lastIndex = pts, index
			if i == 0 && !isRecoveryFrame(au, capture.FormatH264) {
				continue // Already queued waiting-video pictures.
			}
			if i != 0 && pts != sourcePTS+uint32(i*1500) {
				t.Fatal("source intervals changed after waiting-video transition")
			}
			f, err := decoder.Decode(au)
			if err != nil || f == nil {
				t.Fatalf("portrait picture %d cannot decode: %v", i, err)
			}
			color := append([]byte(nil), f.Data[:3]...)
			w, h := f.Width, f.Height
			f.Release()
			if i == 0 && w == 1920 && h == 1080 {
				continue // A waiting-video IDR already queued before capture.
			}
			if i == 0 {
				sourcePTS = pts
			}
			if w != 500 || h != 1080 || color[1+i%2] < 200 || color[2-i%2] > 40 {
				t.Fatalf("portrait picture %d frozen or corrupt: %dx%d BGRA %v", i, w, h, color)
			}
			break
		}
	}
	t.Logf("portrait transition output: %.1f fps", 60/time.Since(motionStart).Seconds())
	select {
	case <-restarts:
		t.Fatal("waiting video restarted the passive source")
	default:
	}
}

// Require real native encoding, UDP reassembly and persistent P-frame decoding.
// An idle AirPlay source must also recover a recreated Moonlight decoder from
// the last decoded picture, without closing the phone or awaiting a source IDR.
func TestPassiveDecodedVideoMotionAndIdleIDR(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_NATIVE_MEDIA") == "" {
		t.Skip("set ANYKVM_TEST_NATIVE_MEDIA and ANYKVM_FFMPEG_LIBDIR")
	}
	x, peer, cancel, restarts := passiveVideoSession(t)
	x.options.Width, x.options.Height = 320, 240
	base := time.Now().Add(-time.Second)
	frame := func(i int) bridge.Frame {
		data := make([]byte, 320*240*4)
		for off := 0; off < len(data); off += 4 {
			data[off+3] = 255
			if i < 4 {
				data[off+2] = 255 // BGRA red.
			} else {
				data[off+1] = 255 // BGRA green.
			}
		}
		return bridge.Frame{PreserveSource: true, Format: capture.FormatRaw,
			Width: 320, Height: 240, Data: data,
			CapturedAt: base.Add(time.Duration(i) * time.Second / 60)}
	}
	done := make(chan error, 1)
	go func() {
		_, err := x.encodeVideo(frame(0), &videoPacketizer{}, base.Add(-time.Millisecond))
		done <- err
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	}()
	decoder, err := avmedia.NewDecoder("h264")
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	receive := func(i int, wantIDR bool, dec *avmedia.Session) {
		t.Helper()
		peer.SetReadDeadline(time.Now().Add(3 * time.Second))
		var payload []byte
		var packet [2048]byte
		for {
			n, _, err := peer.ReadFromUDP(packet[:])
			if err != nil || n < 32 {
				t.Fatalf("UDP picture %d: %v (%d bytes)", i, err, n)
			}
			info := binary.LittleEndian.Uint32(packet[28:])
			if info>>12&1023 >= info>>22 { // Parity.
				continue
			}
			if got := binary.BigEndian.Uint32(packet[4:]); got != uint32(90+i*1500) {
				t.Fatalf("source PTS changed: %d", got)
			}
			payload = append(payload, packet[32:n]...)
			if packet[24]&2 != 0 {
				last := int(binary.LittleEndian.Uint16(payload[4:]))
				payload = payload[:len(payload)-(x.options.PacketSize-16)+last]
				break
			}
		}
		if got := isRecoveryFrame(payload[8:], capture.FormatH264); got != wantIDR {
			t.Fatalf("picture %d: recovery=%t, want %t", i, got, wantIDR)
		}
		decoded, err := dec.Decode(payload[8:])
		if err != nil || decoded == nil {
			t.Fatalf("picture %d: decode %v", i, err)
		}
		defer decoded.Release()
		if decoded.Width != 320 || decoded.Height != 240 {
			t.Fatal("decoded dimensions changed")
		}
		color := decoded.Data[:3]
		if i < 4 && (color[2] < 200 || color[1] > 40) || i >= 4 && (color[1] < 200 || color[2] > 40) {
			t.Fatalf("picture %d frozen or corrupt: BGRA %v", i, color)
		}
	}
	receive(0, true, decoder)
	for i := 1; i < 8; i++ {
		x.frames <- frame(i)
		receive(i, false, decoder)
	}
	// No further source picture is sent. A fresh decoder must decode this IDR
	// independently, without replaying the phone's original dependency chain.
	fresh, err := avmedia.NewDecoder("h264")
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	x.keyframe <- struct{}{}
	receive(7, true, fresh)
	x.frames <- frame(8)
	receive(8, false, fresh)
	select {
	case <-restarts:
		t.Fatal("IDR recovery restarted AirPlay")
	default:
	}
}
