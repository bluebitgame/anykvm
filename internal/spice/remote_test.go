package spice

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"anykvm/internal/capture"
)

func TestRemoteEndpoints(t *testing.T) {
	for _, raw := range []string{"spice://vm.example:5900", "spice://192.0.2.1:5901", "spice://[::1]:5900", "SPICE://localhost:5900"} {
		e, err := parseEndpoint(raw)
		if err != nil || !e.remote() || e.path != "" {
			t.Fatalf("%s: %+v %v", raw, e, err)
		}
		if err := ValidateSource(raw, Options{PasswordEnv: "SPICE_PASSWORD"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{"spice://host", "spice://host:", "spice://host:0", "spice://host:65536", "spice://host:abc", "spice://:5900", "spice://user:secret@host:5900", "spice://host:5900/a", "spice://host:5900?password=secret", "spice://host:5900#part", "spice://[::1]"} {
		if err := ValidateSource(raw, Options{}); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func remoteFixture(t *testing.T) (*remoteDevice, *fakeSession) {
	t.Helper()
	s := &fakeSession{}
	d := &Device{raw: "spice://vm.example:5900", openSession: func(address, password, node string) (nativeSession, error) {
		if address != "vm.example:5900" || node != "" {
			t.Fatalf("remote attempted GPU setup: %s %s", address, node)
		}
		return s, nil
	}}
	remote := wrapRemote(d)
	if err := remote.OpenWithFormat(0, 0, 30); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { remote.Close() })
	return remote, s
}

func enqueueBitmap(s *fakeSession, width, height int, pixels []byte) {
	s.leases = append(s.leases, &frameLease{bitmap: &bitmapFrame{pixels: pixels, width: width, height: height}, done: func() { s.ack++ }})
}

func TestRemoteBitmapOwnershipPacingResizeAndIdle(t *testing.T) {
	d, s := remoteFixture(t)
	var dev capture.Device = d
	if _, direct := dev.(capture.VideoStreamConfigurer); direct {
		t.Fatal("remote falsely advertises direct GameStream encoding")
	}
	if d.SupportsVideoCodec("h264") || !d.SupportsVideoCodec("rawvideo") {
		t.Fatal("incorrect remote codec capabilities")
	}
	pixels := []byte{1, 2, 3, 255}
	enqueueBitmap(s, 1, 1, pixels)
	f, err := d.Capture()
	if err != nil || f == nil || f.Format != capture.FormatRaw || !bytes.Equal(f.Data, pixels) {
		t.Fatalf("bitmap: %+v %v", f, err)
	}
	f.Data[0] = 99
	f.Release()
	d.RequestKeyframe()
	f, err = d.Capture()
	if err != nil || f == nil || f.Data[0] != 1 {
		t.Fatalf("idle snapshot was mutated: %+v %v", f, err)
	}
	f.Release()
	enqueueBitmap(s, 2, 1, []byte{4, 5, 6, 255, 7, 8, 9, 255})
	if f, err = d.Capture(); err != nil || f != nil || len(s.leases) != 1 {
		t.Fatal("pacing consumed final update")
	}
	d.source.lastFrame = time.Now().Add(-time.Second)
	f, err = d.Capture()
	if err != nil || f == nil || f.Width != 2 || d.Stats().GPUFrames != 0 || d.Stats().BitmapFrames != 2 {
		t.Fatalf("resize: %+v %v", f, err)
	}
	f.Release()
	if s.ack != 2 {
		t.Fatalf("snapshot cleanup: %d", s.ack)
	}
	if err := d.MouseMove(32767, 32767); err != nil {
		t.Fatal(err)
	}
	d.Close()
	if d.source.lastBitmap != nil {
		t.Fatal("cached bitmap survived close")
	}
	if _, err = d.Capture(); !errors.Is(err, capture.ErrStreamStopped) {
		t.Fatalf("closed capture: %v", err)
	}
}

func TestRemoteTimeoutAndFailure(t *testing.T) {
	d, s := remoteFixture(t)
	d.source.bitmapWaitAt = time.Now().Add(-time.Minute)
	if _, err := d.Capture(); !errors.Is(err, capture.ErrStreamStopped) {
		t.Fatalf("first bitmap timeout: %v", err)
	}
	d.source.bitmapWaitAt = time.Now()
	s.err = errors.New("connection closed")
	if _, err := d.Capture(); !errors.Is(err, capture.ErrStreamStopped) {
		t.Fatalf("disconnect: %v", err)
	}
	s.err = nil
	enqueueBitmap(s, 2, 2, []byte{1})
	if _, err := d.Capture(); !errors.Is(err, capture.ErrStreamStopped) || s.ack != 1 {
		t.Fatalf("malformed bitmap: %v ack=%d", err, s.ack)
	}
}

func TestRemoteMediaCapabilities(t *testing.T) {
	d, s := remoteFixture(t)
	audio := d.AudioDevice()
	if err := audio.Open(); err != nil {
		t.Fatal(err)
	}
	defer audio.Close()
	s.audioActive = true
	s.audioQueue = []audioPacket{packet(123, -123)}
	if f, err := audio.Capture(); err != nil || f == nil {
		t.Fatalf("remote audio: %+v %v", f, err)
	}
	s.cursorState = &cursorSnapshot{known: true, visible: true, systemDefault: true}
	if c, err := d.CaptureCursor(); err != nil || !c.SystemDefault {
		t.Fatalf("remote cursor: %+v %v", c, err)
	}
	if err := d.KeyDown(4); err != nil {
		t.Fatal(err)
	}
	d.Close()
	if s.inputs != 2 {
		t.Fatalf("remote held key cleanup: %d", s.inputs)
	}
}

func TestRemotePrimaryRemovalInvalidatesIdleSnapshot(t *testing.T) {
	d, s := remoteFixture(t)
	enqueueBitmap(s, 1, 1, []byte{1, 2, 3, 255})
	frame, err := d.Capture()
	if err != nil || frame == nil {
		t.Fatalf("initial bitmap: %v", err)
	}
	defer frame.Release()
	d.source.bitmapWaitAt = time.Now().Add(-time.Minute)
	s.leases = append(s.leases, &frameLease{reset: true, done: func() { s.ack++ }})
	d.RequestKeyframe()
	if f, err := d.Capture(); err != nil || f != nil {
		t.Fatalf("removed primary emitted frame: %+v %v", f, err)
	}
	if d.source.lastBitmap != nil || d.source.width != 0 || d.source.height != 0 {
		t.Fatal("removed primary retained image or input geometry")
	}
	if f, err := d.Capture(); err != nil || f != nil {
		t.Fatalf("refresh resurrected stale primary: %+v %v", f, err)
	}
	// A replacement primary starts a new wait window and does not mutate any
	// outstanding snapshot from the previous surface.
	enqueueBitmap(s, 2, 1, []byte{4, 5, 6, 255, 7, 8, 9, 255})
	next, err := d.Capture()
	if err != nil || next == nil || next.Width != 2 {
		t.Fatalf("replacement primary: %+v %v", next, err)
	}
	next.Release()
	if !bytes.Equal(frame.Data, []byte{1, 2, 3, 255}) {
		t.Fatal("primary replacement mutated outstanding frame")
	}
	s.leases = append(s.leases, &frameLease{reset: true, done: func() {}})
	d.RequestKeyframe()
	if f, err := d.Capture(); err != nil || f != nil {
		t.Fatalf("second removal: %+v %v", f, err)
	}
	d.source.bitmapWaitAt = time.Now().Add(-time.Minute)
	if _, err := d.Capture(); !errors.Is(err, capture.ErrStreamStopped) {
		t.Fatalf("replacement timeout: %v", err)
	}
}

func TestRemoteSettingsDoNotExposeOrUseDMABUFEncoding(t *testing.T) {
	d, s := remoteFixture(t)
	var device any = d
	if _, ok := device.(capture.VideoStreamConfigurer); ok {
		t.Fatal("remote exposed direct video encoding")
	}
	if _, ok := device.(capture.GPUSurfaceConfigurer); ok {
		t.Fatal("remote exposed GPU snapshots")
	}
	if _, ok := device.(capture.JPEGRegionsConfigurer); ok {
		t.Fatal("remote exposed GPU JPEG regions")
	}
	if _, ok := device.(interface{ SupportsGameStreamHEVC() bool }); ok {
		t.Fatal("remote exposed local encoder capabilities")
	}
	d.source.newPipeline = func(*capture.DMABUFEncoder) (localPipeline, error) {
		t.Fatal("remote opened DMA-BUF pipeline")
		return nil, nil
	}
	d.SetDecodeImage(false)
	d.SetJPEGQuality(90)
	d.SetVideoCodec("rawvideo")
	enqueueBitmap(s, 1, 1, []byte{1, 2, 3, 255})
	frame, err := d.Capture()
	if err != nil || frame == nil {
		t.Fatalf("remote capture after configuration: %v", err)
	}
	defer frame.Release()
	if frame.Format != capture.FormatRaw || d.source.activePipeline != nil || s.ack != 1 || d.Stats().GPUFrames != 0 {
		t.Fatal("remote switched away from owned BGRA capture")
	}
}
