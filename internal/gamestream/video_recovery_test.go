//go:build cgo

package gamestream

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
)

func TestDirectRecoveryAfterLatePeerOrIgnoredFeedback(t *testing.T) {
	for _, latePeer := range []bool{true, false} {
		name := "ignored feedback"
		if latePeer {
			name = "late video endpoint"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			socket, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer socket.Close()
			peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			requests := make(chan struct{}, 4)
			restarts := make(chan struct{}, 4)
			s := &Server{videoSocket: socket, cfg: Config{
				OnVideoKeyframe: func() (bool, error) { requests <- struct{}{}; return true, nil },
				OnVideoRestart:  func() error { restarts <- struct{}{}; return nil },
			}}
			x := &streamSession{server: s, ctx: ctx, started: true, frames: make(chan bridge.Frame, 1), keyframe: make(chan struct{}, 1), options: streamOptions{Codec: "hevc", Width: 1328, Height: 2896, FPS: 30, Bitrate: 5000, PacketSize: 1024, MinFEC: 2}}
			s.session = x
			if !latePeer {
				x.videoPeer = peer.LocalAddr().(*net.UDPAddr)
			}
			key := bridge.Frame{Format: capture.FormatHEVC, Width: 1328, Height: 2896, Data: hevcNALs(32, 33, 34, 19)}
			first := key
			if !latePeer {
				first.Data = hevcNALs(1)
			}
			done := make(chan error, 1)
			go func() { _, err := x.streamEncodedVideo(first, &videoPacketizer{}, time.Now()); done <- err }()
			defer func() {
				cancel()
				if err := <-done; err != nil {
					t.Error(err)
				}
			}()
			if latePeer {
				// Let the initial recovery frame be consumed while the client's
				// video endpoint is still unknown.
				time.Sleep(150 * time.Millisecond)
				select {
				case <-requests:
					t.Fatal("request sent before peer ready")
				default:
				}
				x.mu.Lock()
				x.videoPeer = peer.LocalAddr().(*net.UDPAddr)
				x.mu.Unlock()
			}
			select {
			case <-requests:
			case <-time.After(time.Second):
				t.Fatal("no recovery requested once peer ready")
			}
			if !latePeer {
				// The native callback reports success, but no IRAP arrives. A
				// bounded restart must still happen rather than waiting forever.
				select {
				case <-restarts:
				case <-time.After(3 * time.Second):
					t.Fatal("ignored feedback did not cause restart")
				}
			}
			s.PushFrame(key)
			peer.SetReadDeadline(time.Now().Add(time.Second))
			packet := make([]byte, 2048)
			n, _, err := peer.ReadFromUDP(packet)
			if err != nil {
				t.Fatal(err)
			}
			if n < 40+len(key.Data) || !bytes.Equal(packet[40:40+len(key.Data)], key.Data) {
				t.Fatal("recovery frame missing or changed")
			}
			select {
			case <-restarts:
				t.Fatal("restart remained pending after recovery")
			case <-time.After(2200 * time.Millisecond):
			}
		})
	}
}
