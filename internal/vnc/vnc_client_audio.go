package vnc

import (
	"encoding/binary"
	"fmt"
	"io"

	"anykvm/internal/capture"
)

// negotiateQEMUAudio is called only after the server confirms -259 support.
// Setting the format before Enable is essential: audio data has no format header.
func (c *VNCClient) negotiateQEMUAudio() error {
	if c.audioNegotiated {
		return nil
	}
	// Send SetFormat followed by Enable under the same write lock.
	buf := make([]byte, 14)
	formatMessage, enableMessage := buf[:10], buf[10:]
	formatMessage[0], formatMessage[1] = msgQEMU, submsgQEMUAudio
	binary.BigEndian.PutUint16(formatMessage[2:], qemuAudioSetFmt)
	formatMessage[4], formatMessage[5] = qemuAudioS16, capture.AudioChannels
	binary.BigEndian.PutUint32(formatMessage[6:], capture.AudioSampleRate)
	enableMessage[0], enableMessage[1] = msgQEMU, submsgQEMUAudio
	binary.BigEndian.PutUint16(enableMessage[2:], qemuAudioEnable)
	if err := c.writeMessage(buf); err != nil {
		return err
	}
	c.audioNegotiated = true
	return nil
}

func (c *VNCClient) handleQEMUAudioMessage() error {
	var opBuf [2]byte
	if _, err := io.ReadFull(c.reader, opBuf[:]); err != nil {
		return err
	}
	switch op := binary.BigEndian.Uint16(opBuf[:]); op {
	case qemuAudioStart:
		c.audioStarted = c.audioNegotiated
	case qemuAudioEnd:
		c.audioStarted = false
		c.audioMu.Lock()
		c.audioBuf = nil
		c.audioMu.Unlock()
	case qemuAudioData:
		pcm, err := c.readSizedPayload(maxVNCPayload)
		if err != nil {
			return err
		}
		if !c.audioStarted {
			break // Drain late or unsolicited data without misinterpreting its format.
		}
		if len(pcm)%(capture.AudioChannels*pcmBytesPerSample) != 0 {
			return fmt.Errorf("vnc: unaligned QEMU audio payload: %d", len(pcm))
		}
		c.deliverAudioPCM(pcm)
	default:
		return fmt.Errorf("vnc: unsupported audio operation %d", op)
	}
	return nil
}

// deliverAudioPCM either notifies the consumer or retains at most one second.
// Callbacks run outside audioMu so consumers can access the audio device.
func (c *VNCClient) deliverAudioPCM(pcm []byte) {
	c.audioMu.Lock()
	cb := c.audioCb
	if cb == nil {
		// Keep at most one second when no audio consumer is draining.
		const maxBufferedAudio = capture.AudioSampleRate * capture.AudioChannels * pcmBytesPerSample
		c.audioBuf = append(c.audioBuf, pcm...)
		if len(c.audioBuf) > maxBufferedAudio {
			c.audioBuf = append([]byte(nil), c.audioBuf[len(c.audioBuf)-maxBufferedAudio:]...)
		}
	}
	c.audioMu.Unlock()
	if cb != nil {
		cb(pcm)
	}
}

func (c *VNCClient) SetAudioCallback(cb func([]byte)) {
	c.audioMu.Lock()
	c.audioCb = cb
	if cb != nil {
		c.audioBuf = nil
	}
	c.audioMu.Unlock()
}

func (c *VNCClient) audioOpen() error {
	return nil
}

func (c *VNCClient) audioCapture() (*capture.AudioFrame, error) {
	c.audioMu.Lock()
	defer c.audioMu.Unlock()
	if len(c.audioBuf) == 0 {
		return nil, nil
	}
	data := c.audioBuf
	c.audioBuf = nil
	return &capture.AudioFrame{
		PCM:        data,
		SampleRate: capture.AudioSampleRate,
		Channels:   capture.AudioChannels,
	}, nil
}

func (c *VNCClient) audioClose() error {
	return nil
}

type vncAudioSource struct {
	client *VNCClient
}

func (a *vncAudioSource) Open() error                           { return a.client.audioOpen() }
func (a *vncAudioSource) Capture() (*capture.AudioFrame, error) { return a.client.audioCapture() }
func (a *vncAudioSource) Close() error                          { return a.client.audioClose() }

func (c *VNCClient) AudioDevice() capture.AudioDevice {
	return &vncAudioSource{client: c}
}
