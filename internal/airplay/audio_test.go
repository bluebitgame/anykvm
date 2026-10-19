package airplay

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha512"
	"encoding/binary"
	"fmt"
	"net"
	"net/textproto"
	"testing"
	"time"

	"anykvm/internal/capture"
)

func TestAudioPacketQueueDuplicatesReorderingLossAndWrap(t *testing.T) {
	var q audioPacketQueue
	now := time.Now()
	q.push(65534, []byte{1}, now)
	if got := q.pop(now); !bytes.Equal(got, []byte{1}) {
		t.Fatal(got)
	}
	q.push(0, []byte{3}, now)
	q.push(65534, []byte{1}, now)
	if q.pop(now) != nil {
		t.Fatal("played a packet across a gap")
	}
	q.push(65535, []byte{2}, now)
	q.push(0, []byte{3}, now)
	for _, expected := range []byte{2, 3} {
		if got := q.pop(now); !bytes.Equal(got, []byte{expected}) {
			t.Fatal(got)
		}
	}
	if q.pop(now) != nil {
		t.Fatal("decoded redundant RTP twice")
	}
	q.push(2, []byte{5}, now)
	if q.pop(now.Add(29*time.Millisecond)) != nil {
		t.Fatal("did not wait for retransmission")
	}
	if got := q.pop(now.Add(31 * time.Millisecond)); !bytes.Equal(got, []byte{5}) {
		t.Fatal("packet loss stalled audio", got)
	}
	for i := 0; i < 1000; i++ {
		q.push(uint16(100+i), []byte{1}, now)
	}
	if len(q.pending) > 32 {
		t.Fatal("unbounded reorder buffer")
	}
}

func TestAudioUDPDecryptDeliveryTeardownAndReopen(t *testing.T) {
	for _, ct := range []uint64{8, 2} {
		t.Run(fmt.Sprint(ct), func(t *testing.T) { testAudioUDPDecryptDeliveryTeardownAndReopen(t, ct) })
	}
}

func testAudioUDPDecryptDeliveryTeardownAndReopen(t *testing.T, ct uint64) {
	r, err := NewReceiver("airplay:Audio-Test", Options{})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r.start(listener, nil)
	defer r.Close()
	owner := &pairing{}
	r.mu.Lock()
	r.owner = owner
	r.mu.Unlock()
	s := &mediaSession{receiver: r, owner: owner, key: bytes.Repeat([]byte{1}, 16), secret: bytes.Repeat([]byte{2}, 32), audioIV: bytes.Repeat([]byte{3}, 16), localIP: net.ParseIP("127.0.0.1"), peerIP: net.ParseIP("127.0.0.1")}
	options, err := (audioOptions{CT: ct}).normalized()
	if err != nil {
		t.Fatal(err)
	}
	dataPort, controlPort, err := s.startAudio(options)
	if err != nil {
		t.Fatal(err)
	}
	defer s.stopAudio()
	audio := (&Device{receiver: r}).AudioDevice()
	if err := audio.Open(); err != nil {
		t.Fatal(err)
	}
	defer audio.Close()
	if !audio.(interface{ AudioAvailable() bool }).AudioAvailable() {
		t.Fatal("source availability not reported")
	}
	data, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: s.localIP, Port: dataPort})
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	control, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: s.localIP, Port: controlPort})
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	key := sha512.Sum512(append(append([]byte(nil), s.key...), s.secret...))
	block, _ := aes.NewCipher(key[:16])
	pcm := bytes.Repeat([]byte{0x10, 0x27, 0xf0, 0xd8}, 9) // 36 bytes: CBC blocks plus clear tail
	packet := make([]byte, 12+len(pcm))
	packet[0], packet[1] = 0x80, 0xe0
	binary.BigEndian.PutUint16(packet[2:4], 65535)
	binary.BigEndian.PutUint32(packet[4:8], 0xffffff00)
	copy(packet[12:], pcm)
	cipher.NewCBCEncrypter(block, s.audioIV).CryptBlocks(packet[12:44], pcm[:32])
	data.Write(packet)
	frame, err := audio.Capture()
	format := options.frameFormat()
	if err != nil || frame == nil || frame.SampleRate != 44100 || frame.Channels != 2 || !bytes.Equal(frame.Encoded, pcm) || len(frame.PCM) != 0 || frame.Codec != format.Codec || !bytes.Equal(frame.Config, format.Config) || frame.FramesPerPacket != format.FramesPerPacket || frame.Gain == nil || *frame.Gain != 1 || frame.Epoch == 0 {
		t.Fatal("capture AAC delivery", frame, err)
	}
	// Control retransmits wrap the same RTP packet, and must be deduplicated.
	control.Write(append([]byte{0x80, 0xd6, 0, 1}, packet...))
	if f, err := audio.Capture(); err != nil || f != nil {
		t.Fatal("duplicate replayed", f, err)
	}
	s.setAudioVolume(-144)
	binary.BigEndian.PutUint16(packet[2:4], 0)
	binary.BigEndian.PutUint32(packet[4:8], uint32(0xffffff00)+uint32(options.SPF)) // timestamp wrap
	data.Write(packet)
	if f, err := audio.Capture(); err != nil || f == nil || f.Timestamp != int64(options.SPF)*1000000/44100 || f.Gain == nil || *f.Gain != 0 {
		t.Fatal("RTP wrap or sender mute lost", f, err)
	}
	s.flushAudio(nil)
	binary.BigEndian.PutUint16(packet[2:4], 1)
	binary.BigEndian.PutUint32(packet[4:8], 5120)
	data.Write(packet)
	if f, err := audio.Capture(); err != nil || f == nil || f.Timestamp != 0 || f.Epoch <= frame.Epoch {
		t.Fatal("FLUSH retained old timestamps or decoder epoch", f, err)
	}
	s.stopAudio()
	if audio.(interface{ AudioAvailable() bool }).AudioAvailable() {
		t.Fatal("source teardown still available")
	}
	// Audio teardown never disconnects the video/control owner.
	if r.owner != owner || r.listener != listener {
		t.Fatal("audio teardown changed video lifetime")
	}
	if _, _, err := s.startAudio(options); err != nil {
		t.Fatal("audio reopen", err)
	}
	if f, err := audio.Capture(); err != nil || f != nil {
		t.Fatal("stale AAC after restart", f, err)
	}
}

func TestAudioCaptureDropsWithoutViewerAndBoundsBacklog(t *testing.T) {
	r := &Receiver{audioFrames: make(chan capture.AudioFrame, 2)}
	owner := &pairing{}
	r.owner = owner
	pcm := []byte{1, 2, 3, 4}
	r.pushAudio(owner, capture.AudioFrame{Encoded: pcm})
	if len(r.audioFrames) != 0 {
		t.Fatal("retained audio before viewer")
	}
	r.audioOpen = true
	for i := byte(0); i < 10; i++ {
		r.pushAudio(owner, capture.AudioFrame{Encoded: []byte{i, 0, i, 0}})
	}
	if len(r.audioFrames) != 2 || (<-r.audioFrames).Encoded[0] != 8 || (<-r.audioFrames).Encoded[0] != 9 {
		t.Fatal("lag retained old AAC")
	}
	r.pushAudio(&pairing{}, capture.AudioFrame{Encoded: pcm})
	if len(r.audioFrames) != 0 {
		t.Fatal("old sender delivered AAC")
	}
}

func TestRetainedSessionCleanupPreservesNewAudio(t *testing.T) {
	old, current := &pairing{}, &pairing{}
	r := &Receiver{owner: current, audioOwner: current, audioOpen: true, audioEpoch: 7, audioFrames: make(chan capture.AudioFrame, 2)}
	r.pushAudio(current, capture.AudioFrame{Encoded: []byte{1, 2, 3, 4}})
	timing, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	s := &mediaSession{receiver: r, owner: old, stop: make(chan struct{}), timing: &timingSession{conn: timing, stop: make(chan struct{})}}
	s.audio = &audioStream{}
	s.flushAudio(nil)
	s.audio = nil
	s.stopAudio()
	s.Close()
	if r.audioEpoch != 7 || r.audioOwner != current || len(r.audioFrames) != 1 {
		t.Fatal("old RTP cleanup discarded the new session's audio")
	}
}

func TestAudioVolumeAndNegotiation(t *testing.T) {
	if audioGain(-144) != 0 || audioGain(0) != 1 || audioGain(-6) < 0.5 || audioGain(-6) > 0.51 {
		t.Fatal("source gain")
	}
	for _, o := range []audioOptions{{CT: 4}, {SPF: 512}, {Format: 1}, {ControlPort: 65536}, {CT: 2, SPF: 480}, {CT: 8, Format: alacAudioFormat}, {CT: 2, SampleRate: 48000}} {
		if _, err := o.normalized(); err == nil {
			t.Fatal("unsupported format accepted", o)
		}
	}
	for _, o := range []audioOptions{{CT: 2}, {Format: alacAudioFormat}, {CT: 2, SPF: 352, Format: alacAudioFormat, SampleRate: 44100}} {
		got, err := o.normalized()
		if err != nil || got.CT != 2 || got.SPF != 352 || got.Format != alacAudioFormat || got.SampleRate != 44100 {
			t.Fatal("standalone ALAC negotiation", got, err)
		}
	}
}

func TestAudioControlVolumeMuteAndFlushSequence(t *testing.T) {
	r := &Receiver{audioFrames: make(chan capture.AudioFrame, 2)}
	s := &mediaSession{receiver: r}
	p := &pairing{verified: true, media: s}
	status, _, _ := r.handle(request{method: "SET_PARAMETER", body: []byte("volume: -144.0\r\n")}, p, nil)
	if status != 200 || audioGain(s.audioVolumeDB) != 0 {
		t.Fatal("mute was not retained before audio SETUP")
	}
	status, body, _ := r.handle(request{method: "GET_PARAMETER", body: []byte("volume\r\n")}, p, nil)
	if status != 200 || !bytes.Contains(body, []byte("-144.000000")) {
		t.Fatal(status, string(body))
	}
	a := &audioStream{}
	a.queue.push(10, []byte{1}, time.Now())
	s.audio = a
	r.audioFrames <- capture.AudioFrame{PCM: []byte{1, 2, 3, 4}}
	status, _, _ = r.handle(request{method: "FLUSH", headers: textproto.MIMEHeader{"Rtp-Info": {"seq=20;rtptime=100"}}}, p, nil)
	if status != 200 || len(r.audioFrames) != 0 || a.queue.next != 20 {
		t.Fatal("FLUSH did not reset AAC and RTP sequence")
	}
	a.queue.push(19, []byte{9}, time.Now())
	if a.queue.pop(time.Now()) != nil {
		t.Fatal("FLUSH played old RTP")
	}
	a.queue.push(20, []byte{2}, time.Now())
	if !bytes.Equal(a.queue.pop(time.Now()), []byte{2}) {
		t.Fatal("FLUSH blocked fresh RTP")
	}
}
