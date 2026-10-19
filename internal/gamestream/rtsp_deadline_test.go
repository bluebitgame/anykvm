package gamestream

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

func TestRTSPResponseAfterExpiredStartupDeadline(t *testing.T) {
	host, peer := net.Pipe()
	defer host.Close()
	defer peer.Close()
	host.SetDeadline(time.Now().Add(-time.Second))
	peer.SetReadDeadline(time.Now().Add(time.Second))
	response := []byte("RTSP/1.0 200 OK\r\nCSeq: 1\r\n\r\n")
	done := make(chan error, 1)
	x := &streamSession{}
	go func() { done <- x.writeRTSP(host, response) }()
	got := make([]byte, len(response))
	if _, err := io.ReadFull(peer, got); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, response) {
		t.Fatal("changed response")
	}
}
