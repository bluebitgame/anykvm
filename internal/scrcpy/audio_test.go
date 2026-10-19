package scrcpy

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"

	"anykvm/internal/capture"
)

func TestAudioPacket(t *testing.T) {
	for _, modern := range []bool{false, true} {
		var b bytes.Buffer
		writeWords(&b, 0, 123456, 8)
		pcm := []byte{1, 0, 2, 0, 3, 0, 4, 0}
		b.Write(pcm)
		f, err := readAudioPacket(&b, modern)
		if err != nil || f == nil || !bytes.Equal(f.PCM, pcm) || f.Timestamp != 123456 || f.SampleRate != 48000 || f.Channels != 2 {
			t.Fatalf("modern=%v: frame=%+v err=%v", modern, f, err)
		}
		for _, size := range []uint32{0, 3, 1<<20 + 4, 4} {
			b.Reset()
			writeWords(&b, 0, 0, size)
			if _, err := readAudioPacket(&b, modern); err == nil {
				t.Fatalf("accepted bad PCM size %d", size)
			}
		}
	}
}

func TestAudioReceiverDoesNotBlockWithoutSubscriber(t *testing.T) {
	local, remote := net.Pipe()
	defer remote.Close()
	frames := make(chan *capture.AudioFrame, 5)
	go receiveAudio(local, frames, 7, true)
	remote.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if err := binary.Write(remote, binary.BigEndian, uint32(codecIDRaw)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		var b bytes.Buffer
		writeWords(&b, 0, uint32(i), 4)
		writeWords(&b, uint32(i))
		if _, err := remote.Write(b.Bytes()); err != nil {
			t.Fatalf("audio socket stalled without consumer: %v", err)
		}
	}
	remote.Close()
	var last *capture.AudioFrame
	for f := range frames {
		last = f
	}
	if last == nil || last.Timestamp != 19 || last.Epoch != 7 {
		t.Fatalf("lost latest audio block: %+v", last)
	}
}

func TestAudioDisabledKeepsVideoSession(t *testing.T) {
	for _, code := range []uint32{0, 1, 0x616163} {
		local, remote := net.Pipe()
		frames := make(chan *capture.AudioFrame, 1)
		sess := NewSession(Options{Audio: true})
		sess.running = true
		sess.audioFrames = frames
		go receiveAudio(local, frames, 1, true)
		_ = binary.Write(remote, binary.BigEndian, code)
		remote.Close()
		select {
		case _, ok := <-frames:
			if ok {
				t.Fatal("unexpected PCM from disabled audio")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("disabled audio did not stop")
		}
		if !sess.Running() {
			t.Fatal("audio failure stopped video")
		}
	}
}

func TestAudioSubscriberCloseAndReopen(t *testing.T) {
	sess := NewSession(Options{Audio: true})
	sess.running = true
	sess.audioFrames = make(chan *capture.AudioFrame, 5)
	d := NewCapture(sess).AudioDevice()
	if err := d.Open(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := d.Capture(); done <- err }()
	d.Close()
	select {
	case err := <-done:
		if !errors.Is(err, capture.ErrStreamStopped) {
			t.Fatalf("closed subscriber read: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock Capture")
	}
	// A reopened capture session supplies a new queue and epoch.
	sess.audioFrames = make(chan *capture.AudioFrame, 5)
	if err := d.Open(); err != nil {
		t.Fatal(err)
	}
	sess.audioFrames <- &capture.AudioFrame{PCM: []byte{1, 2, 3, 4}, Epoch: 2}
	f, err := d.Capture()
	if err != nil || f == nil || f.Epoch != 2 {
		t.Fatalf("reopen audio=%+v err=%v", f, err)
	}
	d.Close()
	close(sess.audioFrames)
	if err := d.Open(); !errors.Is(err, capture.ErrStreamStopped) {
		t.Fatalf("closed source opened: %v", err)
	}
	if NewCapture(NewSession(Options{})).AudioDevice() != nil {
		t.Fatal("disabled audio exposed a device")
	}
}
