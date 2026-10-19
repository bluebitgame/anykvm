package scrcpy

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
)

func writeWords(b *bytes.Buffer, words ...uint32) {
	for _, word := range words {
		_ = binary.Write(b, binary.BigEndian, word)
	}
}

func TestVideoProtocols(t *testing.T) {
	for _, version := range []string{"3.0", "3.3", "3.3.4", "4.0", "4.1", "5.0", "5.0.1"} {
		t.Run(version, func(t *testing.T) {
			modern := !versionLess(parseVersionParts(version), []int{4, 0})
			var stream bytes.Buffer
			writeWords(&stream, codecIDH264)
			if modern {
				writeWords(&stream, 0x80000000)
			}
			writeWords(&stream, 1920, 1080)
			configFlag, keyFlag := uint32(0x80000000), uint32(0x40000000)
			if modern {
				configFlag, keyFlag = 0x40000000, 0x20000000
				// A later capture session changes dimensions without a payload.
				writeWords(&stream, 0x80000001, 1080, 1920)
			}
			writeWords(&stream, configFlag, 0, 4)
			stream.Write([]byte{0, 0, 1, 0x67})
			writeWords(&stream, keyFlag, 123, 4)
			stream.Write([]byte{0, 0, 1, 0x65})
			reader := bufio.NewReader(&stream)
			codec, w, h, err := readVideoHeader(reader, modern)
			if err != nil || codec != codecIDH264 || w != 1920 || h != 1080 {
				t.Fatalf("header = %x, %dx%d, %v", codec, w, h, err)
			}
			sess := NewSession(Options{})
			sess.vr, sess.sessionPackets = reader, modern
			sess.setSize(w, h)
			for i, wantConfig := range []bool{true, false} {
				data, config, err := sess.ReadPacket()
				want := byte(0x67)
				if i == 1 {
					want = 0x65
				}
				if err != nil || config != wantConfig || !bytes.Equal(data, []byte{0, 0, 1, want}) {
					t.Fatalf("packet %d = %x, config=%v, err=%v", i, data, config, err)
				}
			}
			if modern {
				if w, h := sess.Size(); w != 1080 || h != 1920 {
					t.Fatalf("rotation size = %dx%d", w, h)
				}
			}
			if _, _, err := sess.ReadPacket(); err != io.EOF {
				t.Fatalf("end of stream = %v", err)
			}
		})
	}
}

func TestInvalidVideoSession(t *testing.T) {
	for _, words := range [][]uint32{
		{0, 1920, 1080}, // Missing session flag.
		{0x80000000, 0, 1080},
		{0x80000000, 1920, 65536},
	} {
		var b bytes.Buffer
		writeWords(&b, codecIDH264)
		writeWords(&b, words...)
		if _, _, _, err := readVideoHeader(&b, true); err == nil {
			t.Fatalf("accepted invalid session %x", words)
		}
	}
	for n := 0; n < 16; n++ {
		if _, _, _, err := readVideoHeader(bytes.NewReader(make([]byte, n)), true); err == nil {
			t.Fatalf("accepted truncated header of length %d", n)
		}
	}
}

func TestWheelProtocolRange(t *testing.T) {
	for _, modern := range []bool{false, true} {
		for _, delta := range []int{-1, 0, 1} {
			local, remote := net.Pipe()
			sess := NewSession(Options{})
			sess.control, sess.running, sess.scroll16 = local, true, modern
			sess.setSize(1920, 1080)
			done := make(chan error, 1)
			go func() { done <- sess.MouseWheel(delta) }()
			var packet [21]byte
			_, err := io.ReadFull(remote, packet[:])
			local.Close()
			remote.Close()
			if err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			unit := 32767
			if modern {
				unit = 2048
			}
			if got := int16(binary.BigEndian.Uint16(packet[15:17])); got != int16(delta*unit) {
				t.Fatalf("modern=%v delta=%d: scroll=%d", modern, delta, got)
			}
		}
	}
}

func TestInvalidVideoPackets(t *testing.T) {
	for _, modern := range []bool{false, true} {
		for _, size := range []uint32{0, maxPacketSize + 1, 4} {
			var stream bytes.Buffer
			writeWords(&stream, 0, 123, size) // No payload: size=4 is truncated.
			sess := NewSession(Options{})
			sess.vr, sess.sessionPackets = bufio.NewReader(&stream), modern
			if _, _, err := sess.ReadPacket(); err == nil {
				t.Fatalf("modern=%v accepted invalid packet size %d", modern, size)
			}
		}
	}
	var stream bytes.Buffer
	writeWords(&stream, 0x80000000, 0, 1080)
	sess := NewSession(Options{})
	sess.vr, sess.sessionPackets = bufio.NewReader(&stream), true
	if _, _, err := sess.ReadPacket(); err == nil {
		t.Fatal("accepted invalid dimensions in a later session packet")
	}
}
