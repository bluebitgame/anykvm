package scrcpy

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"anykvm/internal/capture"
	"anykvm/internal/logging"
)

// AudioDevice subscribes to the audio socket of this capture session. Keep
// draining that socket even without a subscriber, so audio cannot stall video.
func (c *Capture) AudioDevice() capture.AudioDevice {
	if !c.sess.opts.Audio {
		return nil
	}
	return &audioDevice{sess: c.sess}
}

type audioDevice struct {
	sess   *Session
	mu     sync.Mutex
	frames <-chan *capture.AudioFrame
	done   chan struct{}
}

func (d *audioDevice) Open() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.done != nil {
		return nil
	}
	d.sess.mu.Lock()
	frames, running := d.sess.audioFrames, d.sess.running
	d.sess.mu.Unlock()
	if !running || frames == nil {
		return fmt.Errorf("scrcpy: device audio is not enabled or session is stopped")
	}
	// Do not replay audio buffered before this subscriber opened.
	for {
		select {
		case _, ok := <-frames:
			if !ok {
				return capture.ErrStreamStopped
			}
		default:
			d.frames, d.done = frames, make(chan struct{})
			return nil
		}
	}
}

func (d *audioDevice) Capture() (*capture.AudioFrame, error) {
	d.mu.Lock()
	frames, done := d.frames, d.done
	d.mu.Unlock()
	if done == nil {
		return nil, capture.ErrStreamStopped
	}
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-done:
		return nil, capture.ErrStreamStopped
	case frame, ok := <-frames:
		if !ok {
			return nil, capture.ErrStreamStopped
		}
		return frame, nil
	case <-timer.C:
		return nil, nil
	}
}

func (d *audioDevice) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.done != nil {
		close(d.done)
		d.done, d.frames = nil, nil
	}
	return nil
}

func receiveAudio(conn net.Conn, frames chan *capture.AudioFrame, epoch uint64, modern bool) {
	defer close(frames)
	defer conn.Close()
	r := bufio.NewReader(conn)
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var codec uint32
	if err := binary.Read(r, binary.BigEndian, &codec); err != nil {
		logging.Infof("scrcpy: audio unavailable: %v", err)
		return
	}
	conn.SetReadDeadline(time.Time{})
	if codec == 0 || codec == 1 {
		logging.Infof("scrcpy: device disabled audio (status=%d); continuing video", codec)
		return
	}
	if codec != codecIDRaw {
		logging.Infof("scrcpy: unexpected audio codec 0x%08x; continuing video", codec)
		return
	}
	for {
		frame, err := readAudioPacket(r, modern)
		if err != nil {
			if err != io.EOF && !errors.Is(err, net.ErrClosed) {
				logging.Infof("scrcpy: audio stopped: %v", err)
			}
			return
		}
		if frame == nil {
			continue
		}
		frame.Epoch = epoch
		select {
		case frames <- frame:
		default:
			// Drop the oldest block when the consumer falls behind.
			select {
			case <-frames:
			default:
			}
			select {
			case frames <- frame:
			default:
			}
		}
	}
}

func readAudioPacket(r io.Reader, modern bool) (*capture.AudioFrame, error) {
	var header [12]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	flags := binary.BigEndian.Uint64(header[:8])
	size := binary.BigEndian.Uint32(header[8:])
	if size == 0 || size > 1<<20 || size%4 != 0 {
		return nil, fmt.Errorf("scrcpy: invalid PCM packet size %d", size)
	}
	pcm := make([]byte, size)
	if _, err := io.ReadFull(r, pcm); err != nil {
		return nil, err
	}
	config, mask := packetFlagConfig, packetFlagConfig|packetFlagKeyFrame
	if modern {
		config, mask = packetFlagConfigV4, packetFlagSession|packetFlagConfigV4|uint64(1)<<61
	}
	if flags&config != 0 {
		return nil, nil
	}
	return &capture.AudioFrame{PCM: pcm, SampleRate: capture.AudioSampleRate,
		Channels: capture.AudioChannels, Timestamp: int64(flags &^ mask)}, nil
}
