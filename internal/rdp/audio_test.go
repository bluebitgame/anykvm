package rdp

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"anykvm/internal/capture"
)

func audioTestClient(t *testing.T) (*client, *pointerRecordingConn) {
	t.Helper()
	c := channelTestClient()
	conn := &pointerRecordingConn{packets: make(chan []byte, 4096)}
	c.conn, c.data.audio = conn, 1006
	c.s.active = c
	t.Cleanup(func() { c.close(); c.s.wg.Wait() })
	return c, conn
}

// Parse the actual TPKT/MCS/static-channel framing, including fragmentation.
func readAudioMessage(t *testing.T, conn *pointerRecordingConn) []byte {
	t.Helper()
	var message []byte
	total := 0
	for {
		var packet []byte
		select {
		case packet = <-conn.packets:
		case <-time.After(time.Second):
			t.Fatal("audio message timeout")
		}
		p, fast, err := readPacket(bytes.NewReader(packet))
		if err != nil || fast || len(p) < 10 || be.Uint16(p[6:]) != 1006 {
			t.Fatal("audio sent on wrong transport/channel")
		}
		n, p, err := takePer(p[9:])
		if err != nil || n != len(p) || len(p) < 8 || len(p)-8 > vcChunk {
			t.Fatal("invalid audio fragment")
		}
		flags := le.Uint32(p[4:])
		if flags&1 != 0 {
			if total != 0 {
				t.Fatal("overlapping audio output fragments")
			}
			total = int(le.Uint32(p))
		}
		if total != int(le.Uint32(p)) {
			t.Fatal("audio fragment total changed")
		}
		message = append(message, p[8:]...)
		if flags&2 != 0 {
			if len(message) != total {
				t.Fatal("incomplete audio output")
			}
			return message
		}
	}
}

func clientAudioFormats(flags uint32, version int, formats ...[]byte) []byte {
	body := join(u32(flags), u32(0xffffffff), u32(0x10000), u16(0), u16(len(formats)), []byte{0}, u16(version), []byte{0})
	for _, f := range formats {
		body = append(body, f...)
	}
	return audioPDU(soundFormats, body)
}

func negotiateTestAudio(t *testing.T, c *client, conn *pointerRecordingConn, version int) {
	t.Helper()
	if err := c.startAudio(); err != nil {
		t.Fatal(err)
	}
	readAudioMessage(t, conn)
	if err := c.handleAudio(clientAudioFormats(1, version, pcmAudioFormat())); err != nil {
		t.Fatal(err)
	}
	if version >= 6 {
		if len(conn.packets) != 0 {
			t.Fatal("training preceded quality mode")
		}
		if err := c.handleAudio(audioPDU(soundQuality, join(u16(2), u16(0)))); err != nil {
			t.Fatal(err)
		}
	}
	training := readAudioMessage(t, conn)
	if !bytes.Equal(training[:4], []byte{soundTraining, 0, 4, 0}) || len(training) != 8 || le.Uint16(training[6:]) != 0 {
		t.Fatalf("invalid training: %x", training)
	}
	if err := c.handleAudio(training); err != nil {
		t.Fatal(err)
	}
	if !c.audio.trained {
		t.Fatal("training did not enable audio")
	}
}

func TestAudioChannelAllocationAndOffer(t *testing.T) {
	core := make([]byte, 142)
	le.PutUint16(core[4:], 640)
	le.PutUint16(core[6:], 480)
	le.PutUint16(core[140:], 0x100)
	connect := func(names ...string) (clientData, error) {
		channels := u32(uint32(len(names)))
		for _, name := range names {
			field := make([]byte, 8)
			copy(field, name)
			channels = join(channels, field, u32(0))
		}
		blocks := join(capability(0xc001, core), capability(0xc003, channels))
		gcc := join([]byte("Duca"), perLength(len(blocks)), blocks)
		value := join(berValue([]byte{4}, nil), berValue([]byte{4}, nil), berValue([]byte{1}, nil), berValue([]byte{0x30}, nil), berValue([]byte{0x30}, nil), berValue([]byte{0x30}, nil), berValue([]byte{4}, gcc))
		return parseConnect(join([]byte{2, 0xf0, 0x80}, berValue([]byte{0x7f, 0x65}, value)))
	}
	data, err := connect("rdpsnd", "drdynvc", "cliprdr")
	if err != nil || data.audio != 1004 || data.dvc != 1005 || data.clipboard != 1006 {
		t.Fatalf("audio channel allocation: %+v %v", data, err)
	}
	if _, err := connect("rdpsnd", "rdpsnd", "drdynvc"); err == nil {
		t.Fatal("accepted duplicate audio channel")
	}
	c, conn := audioTestClient(t)
	if err := c.startAudio(); err != nil {
		t.Fatal(err)
	}
	// Independently specified WAVEFORMATEX: PCM/stereo/48000/192000/4/16/no extra.
	want, _ := hex.DecodeString("0700260000000000000000000000000000000100000800000100020080bb000000ee0200040010000000")
	if got := readAudioMessage(t, conn); !bytes.Equal(got, want) {
		t.Fatalf("server formats: %x want %x", got, want)
	}
	if err := c.startAudio(); err != nil || len(conn.packets) != 0 {
		t.Fatal("audio offer repeated")
	}
}

func TestAudioNegotiationDisabledMalformedAndFragmented(t *testing.T) {
	for _, mode := range []string{"no-formats", "disabled", "unsupported", "old-version"} {
		t.Run(mode, func(t *testing.T) {
			c, conn := audioTestClient(t)
			c.startAudio()
			readAudioMessage(t, conn)
			flags, version, formats := uint32(1), 8, [][]byte{pcmAudioFormat()}
			switch mode {
			case "no-formats":
				formats = nil
			case "disabled":
				flags = 0
			case "unsupported":
				formats[0] = bytes.Clone(formats[0])
				formats[0][0] = 2
			case "old-version":
				version = 1
			}
			if err := c.handleAudio(clientAudioFormats(flags, version, formats...)); err != nil {
				t.Fatal(err)
			}
			if err := c.handleAudio(audioPDU(soundTraining, make([]byte, 4))); err != nil {
				t.Fatal(err)
			}
			c.s.PushAudio(capture.AudioFrame{PCM: make([]byte, 1920)})
			if c.audio.trained || len(conn.packets) != 0 || len(c.audio.queue) != 0 {
				t.Fatal("disabled client accepted audio")
			}
		})
	}
	c, conn := audioTestClient(t)
	c.startAudio()
	readAudioMessage(t, conn)
	p := clientAudioFormats(1, 8, pcmAudioFormat())
	first := join(u32(uint32(len(p))), u32(1), p[:13])
	last := join(u32(uint32(len(p))), u32(2), p[13:])
	if err := c.handleAudioVC(first); err != nil {
		t.Fatal(err)
	}
	if len(conn.packets) != 0 {
		t.Fatal("negotiated before final fragment")
	}
	if err := c.handleAudioVC(last); err != nil {
		t.Fatal(err)
	}
	if err := c.handleAudio(audioPDU(soundQuality, join(u16(2), u16(0)))); err != nil {
		t.Fatal(err)
	}
	training := readAudioMessage(t, conn)
	wrong := bytes.Clone(training)
	wrong[4]++
	if err := c.handleAudio(wrong); err != nil || c.audio.trained {
		t.Fatal("accepted incorrect training confirmation")
	}
	if err := c.handleAudio(training); err != nil || !c.audio.trained {
		t.Fatal("valid training confirmation rejected")
	}
	if err := c.handleAudio(audioPDU(soundQuality, join(u16(2), u16(0)))); err != nil {
		t.Fatal(err)
	}
	bad := [][]byte{nil, {7, 0, 20, 0}, audioPDU(soundFormats, make([]byte, 19)), audioPDU(soundConfirm, nil), audioPDU(soundTraining, make([]byte, 3))}
	for _, p := range bad {
		x, _ := audioTestClient(t)
		x.startAudio()
		if err := x.handleAudio(p); err == nil {
			t.Fatalf("accepted malformed audio %x", p)
		}
	}
	for _, p := range [][]byte{nil, join(u32(1<<20), u32(3), []byte{1}), join(u32(4), u32(0x23), make([]byte, 4)), join(u32(4), u32(2), make([]byte, 4)), join(u32(8), u32(3), make([]byte, 4))} {
		x, _ := audioTestClient(t)
		if err := x.handleAudioVC(p); err == nil {
			t.Fatalf("accepted malformed audio envelope %x", p)
		}
	}
}

func TestAudioWaveVersionsAndPCMOwnership(t *testing.T) {
	for _, version := range []uint16{5, 8} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			c, conn := audioTestClient(t)
			pcm := make([]byte, 8192)
			for i := range pcm {
				pcm[i] = byte(i)
			}
			if err := c.sendAudioWave(audioChunk{pcm: pcm, captured: 123456}, version, 3, 255, 65530); err != nil {
				t.Fatal(err)
			}
			p := readAudioMessage(t, conn)
			if le.Uint16(p[4:]) != 65530 || le.Uint16(p[6:]) != 3 || p[8] != 255 {
				t.Fatal("incorrect wave metadata")
			}
			if version >= 8 {
				if p[0] != 13 || int(le.Uint16(p[2:])) != len(p)-4 || le.Uint32(p[12:]) != 123456 || !bytes.Equal(p[16:], pcm) {
					t.Fatal("invalid Wave2 PCM")
				}
			} else {
				data := readAudioMessage(t, conn)
				if p[0] != 2 || le.Uint16(p[2:]) != uint16(8+len(pcm)) || len(p) != 16 || !bytes.Equal(p[12:16], pcm[:4]) || !bytes.Equal(data[:4], make([]byte, 4)) || !bytes.Equal(data[4:], pcm[4:]) {
					t.Fatal("invalid WaveInfo/Wave PCM")
				}
			}
		})
	}
	c, conn := audioTestClient(t)
	negotiateTestAudio(t, c, conn, 8)
	pcm := bytes.Repeat([]byte{10, 20, 30, 40}, 2500)
	c.s.PushAudio(capture.AudioFrame{PCM: pcm, SampleRate: 48000, Channels: 2})
	var queued []byte
	for len(c.audio.queue) > 0 {
		queued = append(queued, (<-c.audio.queue).pcm...)
	}
	pcm[0] = 99
	if len(queued) != len(pcm) || queued[0] != 10 || !bytes.Equal(queued[1:], pcm[1:]) {
		t.Fatal("PCM lost or borrowed caller buffer")
	}
	for i := 0; i < 100; i++ {
		c.s.PushAudio(capture.AudioFrame{PCM: bytes.Repeat([]byte{byte(i), 0, 0, 0}, 960)})
	}
	if len(c.audio.queue) != audioQueueSize || (<-c.audio.queue).pcm[0] != 95 {
		t.Fatal("audio backlog not bounded or newest data dropped")
	}
	c.s.SetAudioAvailable(false)
	if len(c.audio.queue) != 0 || c.audio.generation != 1 {
		t.Fatal("source detach retained old audio")
	}
}

func TestAudioLoopFlowControlPacingAndClose(t *testing.T) {
	c, conn := audioTestClient(t)
	negotiateTestAudio(t, c, conn, 8)
	c.startAudioLoop()
	for i := 0; i < audioWindow; i++ {
		c.s.PushAudio(capture.AudioFrame{PCM: make([]byte, 1920), SampleRate: 48000, Channels: 2})
		p := readAudioMessage(t, conn)
		if p[0] != soundWave2 || p[8] != byte(i+1) {
			t.Fatal("incorrect audio block sequence")
		}
	}
	c.s.PushAudio(capture.AudioFrame{PCM: make([]byte, 1920)})
	select {
	case <-conn.packets:
		t.Fatal("audio exceeded unacknowledged window")
	case <-time.After(25 * time.Millisecond):
	}
	c.audio.mu.Lock()
	pending := c.audio.outstanding[1]
	c.audio.mu.Unlock()
	if err := c.handleAudio(audioPDU(soundConfirm, join(u16(int(pending.stamp+10)), []byte{1, 0}))); err != nil {
		t.Fatal(err)
	}
	p := readAudioMessage(t, conn)
	if p[8] != byte(audioWindow+1) {
		t.Fatal("confirmation did not resume audio")
	}
	// Duplicate and stale confirmations cannot release a reused block ID.
	c.audio.mu.Lock()
	c.audio.outstanding[0] = audioPending{sent: time.Now(), stamp: pending.stamp + 1000}
	c.audio.mu.Unlock()
	c.handleAudio(audioPDU(soundConfirm, join(u16(int(pending.stamp+10)), []byte{0, 0})))
	c.audio.mu.Lock()
	_, kept := c.audio.outstanding[0]
	c.audio.mu.Unlock()
	if !kept {
		t.Fatal("stale confirmation released reused block")
	}
	c.close()
	done := make(chan struct{})
	go func() { c.s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("audio worker did not stop")
	}
}

func TestAudioQualityTimeout(t *testing.T) {
	c, conn := audioTestClient(t)
	c.startAudio()
	readAudioMessage(t, conn)
	if err := c.handleAudio(clientAudioFormats(1, 8, pcmAudioFormat())); err != nil {
		t.Fatal(err)
	}
	if !c.audio.qualityPending || len(conn.packets) != 0 {
		t.Fatal("did not wait for quality mode")
	}
	// Advance only the timeout's reference time, keeping the test fast.
	c.audio.mu.Lock()
	c.audio.formatsAt = time.Now().Add(-time.Second)
	c.audio.mu.Unlock()
	c.startAudioLoop()
	training := readAudioMessage(t, conn)
	if training[0] != soundTraining {
		t.Fatal("quality timeout did not start training")
	}
	if err := c.handleAudio(training); err != nil {
		t.Fatal(err)
	}
	c.audio.mu.Lock()
	trained := c.audio.trained
	c.audio.mu.Unlock()
	if !trained {
		t.Fatal("quality timeout did not enable audio")
	}
}

func FuzzAudioMessages(f *testing.F) {
	f.Add(clientAudioFormats(1, 8, pcmAudioFormat()))
	f.Add(audioPDU(soundConfirm, make([]byte, 4)))
	f.Fuzz(func(t *testing.T, p []byte) {
		c := channelTestClient()
		c.data.audio = 1006
		c.audio.started = true
		_ = c.handleAudio(p)
		_ = c.handleAudioVC(p)
	})
}

// Receipt ACKs may arrive promptly while the playback device falls behind.
// After detecting adjusted second ACKs, only playback releases the PCM budget.
func TestAudioPlaybackConfirmationsBoundClientQueue(t *testing.T) {
	c, conn := audioTestClient(t)
	negotiateTestAudio(t, c, conn, 8)
	a := &c.audio
	now := time.Now()
	a.outstanding[1] = audioPending{sent: now, stamp: 65530, duration: 20 * time.Millisecond}
	a.outstanding[2] = audioPending{sent: now, stamp: 65530, duration: 20 * time.Millisecond}
	a.confirmLocked(1, 65530, now)
	a.confirmLocked(2, 65530, now)
	a.confirmLocked(1, 65530, now) // Duplicate receipt is not playback.
	if a.playbackConfirm || len(a.outstanding) != 0 {
		t.Fatal("receipt ACK detected as playback")
	}
	a.confirmLocked(1, 14, now) // 20ms later across uint16 wrap.
	if !a.playbackConfirm || len(a.outstanding) != 1 {
		t.Fatal("did not restore receipt-ACKed blocks on playback detection")
	}
	for i := byte(3); i <= 6; i++ {
		a.outstanding[i] = audioPending{sent: now, stamp: 50, duration: 20 * time.Millisecond}
		a.confirmLocked(i, 50, now)
	}
	if a.playbackQueuedLocked() < audioPlayWindow {
		t.Fatal("receipt ACKs released the playback budget")
	}
	a.confirmLocked(3, 50, now)
	if a.playbackQueuedLocked() < audioPlayWindow {
		t.Fatal("duplicate receipt released playback budget")
	}
	a.confirmLocked(2, 14, now.Add(3*time.Second))
	if a.playbackQueuedLocked() >= audioPlayWindow {
		t.Fatal("delayed playback ACK did not release budget")
	}
	a.confirmLocked(2, 14, now.Add(3*time.Second))
	if len(a.outstanding) != 4 {
		t.Fatal("duplicate playback changed unrelated blocks")
	}
}

func TestAudioRechecksAgeAfterTransportWait(t *testing.T) {
	for _, version := range []int{5, 8} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			c, conn := audioTestClient(t)
			negotiateTestAudio(t, c, conn, version)
			c.staticWriteMu.Lock()
			locked := true
			defer func() {
				if locked {
					c.staticWriteMu.Unlock()
				}
			}()
			result := make(chan error, 1)
			captured := audioTicks(time.Now())
			go func() {
				sent, err := c.sendQueuedAudio(audioChunk{pcm: make([]byte, audioChunkBytes), captured: captured})
				if err == nil && !sent.IsZero() {
					err = fmt.Errorf("sent audio that expired while waiting for transport")
				}
				result <- err
			}()
			// Ensure even a recently captured chunk expires behind graphics traffic.
			// The sender must check age after obtaining staticWriteMu.
			time.Sleep(audioMaxAge + 30*time.Millisecond)
			c.staticWriteMu.Unlock()
			locked = false
			select {
			case err := <-result:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("sender stuck")
			}
			if len(conn.packets) != 0 || c.audio.nextBlock != 1 || len(c.audio.outstanding) != 0 {
				t.Fatal("dropped PCM consumed a block ID or reached the wire")
			}
		})
	}
}

func TestAudioPlaybackWindowUsesDuration(t *testing.T) {
	a := audioState{playbackConfirm: true, outstanding: map[byte]audioPending{
		1: {duration: 90 * time.Millisecond},
	}}
	if a.playbackQueuedLocked()+10*time.Millisecond > audioPlayWindow {
		t.Fatal("rejected exact playback budget")
	}
	if a.playbackQueuedLocked()+20*time.Millisecond <= audioPlayWindow {
		t.Fatal("allowed PCM exceeding playback budget")
	}
}

func TestAudioLoopStopsAtPlaybackBudget(t *testing.T) {
	c, conn := audioTestClient(t)
	negotiateTestAudio(t, c, conn, 8)
	c.startAudioLoop()
	push := func() { c.s.PushAudio(capture.AudioFrame{PCM: make([]byte, audioChunkBytes)}) }
	confirm := func(p []byte, offset uint16) {
		t.Helper()
		stamp := le.Uint16(p[4:]) + offset
		if err := c.handleAudio(audioPDU(soundConfirm, join(u16(int(stamp)), []byte{p[8], 0}))); err != nil {
			t.Fatal(err)
		}
	}
	push()
	initial := readAudioMessage(t, conn)
	confirm(initial, 0)
	confirm(initial, 20)
	var first []byte
	for i := 0; i < 5; i++ {
		push()
		p := readAudioMessage(t, conn)
		if i == 0 {
			first = p
		}
		confirm(p, 0)
	}
	push()
	select {
	case <-conn.packets:
		t.Fatal("receipt ACK allowed more than 100ms of unplayed audio")
	case <-time.After(25 * time.Millisecond):
	}
	confirm(first, 100)
	if p := readAudioMessage(t, conn); p[8] != initial[8]+6 {
		t.Fatal("playback ACK did not resume consecutive audio blocks")
	}
}

func TestAudioLoopDoesNotBurstAfterTransportStall(t *testing.T) {
	c, conn := audioTestClient(t)
	negotiateTestAudio(t, c, conn, 8)
	c.staticWriteMu.Lock()
	locked := true
	defer func() {
		if locked {
			c.staticWriteMu.Unlock()
		}
	}()
	c.s.PushAudio(capture.AudioFrame{PCM: make([]byte, audioChunkBytes)})
	c.startAudioLoop()
	deadline := time.Now().Add(time.Second)
	for len(c.audio.queue) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("audio worker did not dequeue")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(40 * time.Millisecond)
	c.s.PushAudio(capture.AudioFrame{PCM: make([]byte, audioChunkBytes)})
	c.staticWriteMu.Unlock()
	locked = false
	readAudioMessage(t, conn)
	first := time.Now()
	readAudioMessage(t, conn)
	if elapsed := time.Since(first); elapsed < 15*time.Millisecond {
		t.Fatalf("burst after transport stall: %s between 20ms packets", elapsed)
	}
}
