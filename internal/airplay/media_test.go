package airplay

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"net"
	"strings"
	"testing"
	"time"
)

func TestMirrorIdleResumePreservesEncryptedPictures(t *testing.T) {
	config, idr, want := fixtureAVC(t)
	block, _ := aes.NewCipher(make([]byte, 16))
	encrypt := cipher.NewCTR(block, make([]byte, 16))
	decoder := NewMirrorDecoder(cipher.NewCTR(block, make([]byte, 16)))
	conn := &idleControlConn{data: strings.NewReader("")}
	reader := bufio.NewReader(conn)
	decode := func(kind uint16, payload []byte, idleTimeouts int) []byte {
		t.Helper()
		conn.data = strings.NewReader(string(mirrorWire(kind, payload)))
		conn.timeouts = idleTimeouts
		packet, err := readMirrorPacket(conn, reader, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		au, err := decoder.Decode(packet)
		if err != nil {
			t.Fatal(err)
		}
		return au
	}
	decode(1, config, 0)
	video := binary.BigEndian.AppendUint32(nil, uint32(len(idr)))
	video = append(video, idr...)
	encrypt.XORKeyStream(video, video)
	if au := decode(0, video, 0); !bytes.Equal(au, want) {
		t.Fatal("first picture changed")
	}
	// Simulate multiple idle deadlines while locked, then a repeated codec
	// configuration followed by a delta, with no rotation or new IDR.
	decode(1, config, 3)
	delta := []byte{0, 0, 0, 2, 0x41, 0x80}
	encrypt.XORKeyStream(delta, delta)
	if au := decode(0, delta, 2); !bytes.Equal(au, []byte{0, 0, 0, 1, 0x41, 0x80}) {
		t.Fatal("resumed picture lost or cipher restarted")
	}
}

func TestMirrorPartialPacketTimeoutIsFatal(t *testing.T) {
	wire := mirrorWire(0, []byte{1, 2, 3, 4})
	for _, prefix := range []int{1, 127, 128, 130} {
		conn := &idleControlConn{data: strings.NewReader(string(wire[:prefix]))}
		reader := bufio.NewReader(conn)
		if _, err := reader.Peek(1); err != nil {
			t.Fatal(err)
		}
		conn.timeouts = 1
		if _, err := readMirrorPacket(conn, reader, time.Second); err == nil || conn.deadlines != 2 {
			t.Fatalf("partial packet (%d bytes) retried: %v deadlines=%d", prefix, err, conn.deadlines)
		}
	}
}

type mirrorIdleConn struct {
	net.Conn
	deadlines chan struct{}
}

func (c *mirrorIdleConn) SetReadDeadline(deadline time.Time) error {
	err := c.Conn.SetReadDeadline(deadline)
	select {
	case c.deadlines <- struct{}{}:
	default:
	}
	return err
}

func TestMirrorIdleReadStopsWhenConnectionCloses(t *testing.T) {
	local, peer := net.Pipe()
	defer local.Close()
	defer peer.Close()
	conn := &mirrorIdleConn{Conn: local, deadlines: make(chan struct{}, 8)}
	done := make(chan error, 1)
	go func() {
		_, err := readMirrorPacket(conn, bufio.NewReader(conn), 10*time.Millisecond)
		done <- err
	}()
	// Observe the retry, without sleeping for the production idle timeout.
	for i := 0; i < 2; i++ {
		select {
		case <-conn.deadlines:
		case <-time.After(time.Second):
			t.Fatal("idle read did not retry")
		}
	}
	local.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("closed idle connection returned a packet")
		}
	case <-time.After(time.Second):
		t.Fatal("closing connection did not unblock idle mirror reader")
	}
}
