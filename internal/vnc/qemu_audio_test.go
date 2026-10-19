package vnc

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"anykvm/internal/capture"
)

// Exercise Open against an independent server message sequence. In particular,
// a server without -259 support must never receive a QEMU message.
func TestVNCClientAudioNegotiation(t *testing.T) {
	for _, audio := range []bool{false, true} {
		t.Run(fmt.Sprintf("audio=%v", audio), func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			done := make(chan error, 1)
			pcm := []byte{1, 0, 2, 0}
			go func() {
				done <- func() error {
					conn, err := ln.Accept()
					if err != nil {
						return err
					}
					defer conn.Close()
					conn.SetDeadline(time.Now().Add(5 * time.Second))
					s := &client{conn: conn, server: NewServer(Config{Name: "test", Width: 1, Height: 1}), width: 1, height: 1}
					if err := s.handshake(); err != nil {
						return err
					}
					var pf [20]byte
					if _, err := io.ReadFull(conn, pf[:]); err != nil {
						return err
					}
					if pf[0] != msgSetPixelFormat {
						return fmt.Errorf("expected SetPixelFormat")
					}
					var enc [4]byte
					if _, err := io.ReadFull(conn, enc[:]); err != nil {
						return err
					}
					if enc[0] != msgSetEncodings {
						return fmt.Errorf("expected SetEncodings")
					}
					if _, err := io.CopyN(io.Discard, conn, int64(binary.BigEndian.Uint16(enc[2:]))*4); err != nil {
						return err
					}
					formatted, enabled, frames := false, false, 0
					for frames < 2 {
						var typ [1]byte
						if _, err := io.ReadFull(conn, typ[:]); err != nil {
							return err
						}
						switch typ[0] {
						case msgFramebufferUpdateRequest:
							var request [9]byte
							if _, err := io.ReadFull(conn, request[:]); err != nil {
								return err
							}
							// Do not send the second frame until audio has been negotiated.
							if audio && frames == 1 && !enabled {
								continue
							}
							update := append([]byte{0, 0, 0, 1}, rectangleHeader(0, 0, 1, 1, encRaw)...)
							update = append(update, 1, 2, 3, 255)
							if _, err := conn.Write(update); err != nil {
								return err
							}
							frames++
							if audio && frames == 1 {
								// QEMU sends desktop dimensions, rather than a 0x0 rectangle.
								ack := append([]byte{0, 0, 0, 1}, rectangleHeader(0, 0, 1, 1, encQEMUAudio)...)
								ack = append(ack, ack...) // Duplicate acknowledgements must be harmless.
								if _, err := conn.Write(ack); err != nil {
									return err
								}
							}
						case msgQEMU:
							if !audio {
								return fmt.Errorf("audio command sent to unsupported server")
							}
							var hdr [3]byte
							if _, err := io.ReadFull(conn, hdr[:]); err != nil {
								return err
							}
							if hdr[0] != submsgQEMUAudio {
								return fmt.Errorf("unexpected QEMU submessage")
							}
							switch binary.BigEndian.Uint16(hdr[1:]) {
							case qemuAudioSetFmt:
								if formatted || enabled {
									return fmt.Errorf("duplicate or late SetFormat")
								}
								var f [6]byte
								if _, err := io.ReadFull(conn, f[:]); err != nil {
									return err
								}
								if f[0] != 3 || f[1] != 2 || binary.BigEndian.Uint32(f[2:]) != 48000 {
									return fmt.Errorf("wrong audio format: %v", f)
								}
								formatted = true
							case qemuAudioEnable:
								if !formatted || enabled {
									return fmt.Errorf("Enable without format, or duplicate Enable")
								}
								enabled = true
								packet := []byte{255, 1, 0, 1, 255, 1, 0, 2, 0, 0, 0, 4}
								packet = append(packet, pcm...)
								if _, err := conn.Write(packet); err != nil {
									return err
								}
							default:
								return fmt.Errorf("unexpected audio operation")
							}
						default:
							return fmt.Errorf("unexpected message %d", typ[0])
						}
					}
					return nil
				}()
			}()
			c, err := NewVNCClient("vnc://" + ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Open(); err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			for i := 0; i < 2; i++ {
				select {
				case frame := <-c.frameCh:
					if !bytes.Equal(frame.Data, []byte{1, 2, 3, 255}) {
						t.Fatalf("wrong frame: %v", frame.Data)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("video stalled")
				}
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			a, err := c.audioCapture()
			if err != nil {
				t.Fatal(err)
			}
			if audio && (a == nil || !bytes.Equal(a.PCM, pcm)) {
				t.Fatalf("unexpected audio: %+v", a)
			}
			if !audio && a != nil {
				t.Fatal("unexpected audio from unsupported server")
			}
		})
	}
}

// Memory connection lets control and PCM output be checked without timing sleeps.
type audioTestConn struct {
	net.Conn
	input  *bytes.Reader
	output bytes.Buffer
}

func (c *audioTestConn) Read(p []byte) (int, error)       { return c.input.Read(p) }
func (c *audioTestConn) Write(p []byte) (int, error)      { return c.output.Write(p) }
func (c *audioTestConn) SetWriteDeadline(time.Time) error { return nil }
func (c *audioTestConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }

func TestQEMUAudioServerLifecycle(t *testing.T) {
	conn := &audioTestConn{}
	c := &client{conn: conn}
	control := func(op uint16, body ...byte) {
		b := binary.BigEndian.AppendUint16(nil, op)
		b = append(b, body...)
		conn.input = bytes.NewReader(b)
		c.handleQEMUAudio()
	}
	frame := capture.AudioFrame{PCM: []byte{0, 0, 0, 0, 0, 128, 0, 128}, SampleRate: 48000, Channels: 2}
	control(qemuAudioEnable)
	c.pushQEMUAudio(frame)
	if conn.output.Len() != 0 {
		t.Fatal("audio before capability negotiation")
	}
	c.setQEMUAudioCapability(true)
	wantAck := append([]byte{0, 0, 0, 1}, rectangleHeader(0, 0, 0, 0, encQEMUAudio)...)
	if !bytes.Equal(conn.output.Bytes(), wantAck) {
		t.Fatalf("bad capability ack %v", conn.output.Bytes())
	}
	conn.output.Reset()
	control(qemuAudioEnable)
	if conn.output.Len() != 0 {
		t.Fatal("audio started before format set")
	}
	// U8 mono at 48kHz: silence=128 and minimum=0.
	control(qemuAudioSetFmt, 0, 1, 0, 0, 187, 128)
	control(qemuAudioEnable)
	c.pushQEMUAudio(frame)
	control(qemuAudioDisable)
	c.pushQEMUAudio(frame)
	want := []byte{255, 1, 0, 1, 255, 1, 0, 2, 0, 0, 0, 2, 128, 0, 255, 1, 0, 0}
	if !bytes.Equal(conn.output.Bytes(), want) {
		t.Fatalf("stream: %v, want %v", conn.output.Bytes(), want)
	}
	for _, f := range [][]byte{{6, 2, 0, 0, 187, 128}, {3, 0, 0, 0, 187, 128}, {3, 2, 0, 0, 0, 0}, {3, 2, 255, 255, 255, 255}} {
		conn.output.Reset()
		control(qemuAudioSetFmt, f...)
		control(qemuAudioEnable)
		c.pushQEMUAudio(frame)
		if conn.output.Len() != 0 {
			t.Fatalf("invalid format sent audio: %v", f)
		}
	}
	control(qemuAudioSetFmt, 3, 2, 0, 0, 187, 128)
	control(qemuAudioEnable)
	conn.output.Reset()
	c.setQEMUAudioCapability(false)
	c.pushQEMUAudio(frame)
	if !bytes.Equal(conn.output.Bytes(), []byte{255, 1, 0, 0}) {
		t.Fatal("capability removal did not stop audio")
	}
}

func TestQEMUAudioSampleFormats(t *testing.T) {
	pcm := []byte{0, 128, 0, 0, 255, 127}
	wants := [][]byte{{0, 128, 255}, {128, 0, 127}, {0, 0, 0, 128, 255, 255}, pcm, {0, 0, 0, 0, 0, 0, 0, 128, 0, 0, 255, 255}, {0, 0, 0, 128, 0, 0, 0, 0, 0, 0, 255, 127}}
	for format, want := range wants {
		var s qemuAudioConverter
		got := s.convert(capture.AudioFrame{PCM: pcm, SampleRate: 48000, Channels: 1}, qemuAudioFormat{byte(format), 1, 48000})
		if !bytes.Equal(got, want) {
			t.Fatalf("format %d: %v, want %v", format, got, want)
		}
	}
}

func TestQEMUAudioResamplingAcrossPackets(t *testing.T) {
	pcm := make([]byte, 4800*4)
	for i := 0; i < len(pcm)/2; i++ {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(i))
	}
	format := qemuAudioFormat{3, 1, 44100}
	var whole, split qemuAudioConverter
	a := capture.AudioFrame{PCM: pcm, SampleRate: 48000, Channels: 2}
	want := whole.convert(a, format)
	var got []byte
	for offset := 0; offset < len(pcm); offset += 28 {
		a.PCM = pcm[offset:min(offset+28, len(pcm))]
		got = append(got, split.convert(a, format)...)
	}
	if len(want) != 4410*2 || !bytes.Equal(got, want) {
		t.Fatalf("resampling changed at packet boundaries: %d / %d", len(got), len(want))
	}
	a.PCM = pcm
	a.Epoch = 1
	if reset := split.convert(a, format); !bytes.Equal(reset, want) {
		t.Fatal("source restart did not reset resampling")
	}
}

func TestVNCClientAudioStreamState(t *testing.T) {
	c := &VNCClient{audioNegotiated: true}
	message := func(op uint16, pcm []byte) error {
		b := binary.BigEndian.AppendUint16([]byte{submsgQEMUAudio}, op)
		if op == qemuAudioData {
			b = binary.BigEndian.AppendUint32(b, uint32(len(pcm)))
			b = append(b, pcm...)
		}
		c.reader = bytes.NewReader(b)
		return c.handleQEMUMessage()
	}
	pcm := []byte{1, 0, 2, 0}
	if err := message(qemuAudioData, pcm); err != nil {
		t.Fatal(err)
	}
	if len(c.audioBuf) != 0 {
		t.Fatal("data accepted before Start")
	}
	if err := message(qemuAudioStart, nil); err != nil {
		t.Fatal(err)
	}
	if err := message(qemuAudioData, pcm); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(c.audioBuf, pcm) {
		t.Fatal("active stream data lost")
	}
	if err := message(qemuAudioEnd, nil); err != nil {
		t.Fatal(err)
	}
	if err := message(qemuAudioData, pcm); err != nil {
		t.Fatal(err)
	}
	if len(c.audioBuf) != 0 {
		t.Fatal("stale audio remained after End")
	}
	if err := message(qemuAudioStart, nil); err != nil {
		t.Fatal(err)
	}
	if err := message(qemuAudioData, []byte{1}); err == nil {
		t.Fatal("partial sample frame accepted")
	}
}
