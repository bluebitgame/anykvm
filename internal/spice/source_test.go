package spice

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"anykvm/internal/capture"
	"anykvm/internal/dmabuf"
)

type fakeSession struct {
	leases               []*frameLease
	ack, inputs          int
	closed               bool
	err                  error
	audioQueue           []audioPacket
	audioOn, audioActive bool
	audioErr             error
	cursorState          *cursorSnapshot
}

func (s *fakeSession) next() (*frameLease, error) {
	if s.err != nil {
		return nil, s.err
	}
	if len(s.leases) == 0 {
		return nil, nil
	}
	f := s.leases[0]
	s.leases = s.leases[1:]
	return f, nil
}
func (s *fakeSession) input(int, int, int, int) error { s.inputs++; return nil }
func (s *fakeSession) close()                         { s.closed = true }

type fakeEncoder struct {
	frames, repeats   int
	err               error
	inEncode, release chan struct{}
	closeCalled       bool
}

func (e *fakeEncoder) EncodeSource(f dmabuf.Frame, force bool) ([]byte, error) {
	e.frames++
	if e.inEncode != nil {
		close(e.inEncode)
		<-e.release
	}
	return []byte{0, 0, 0, 1, 0x65}, e.err
}
func (e *fakeEncoder) EncodeLast(force bool) ([]byte, error) {
	e.repeats++
	return []byte{0, 0, 0, 1, 0x65}, e.err
}
func (e *fakeEncoder) Close() { e.closeCalled = true }
func fixture(t *testing.T) (*Device, *fakeSession, *fakeEncoder) {
	t.Helper()
	s, e := &fakeSession{}, &fakeEncoder{}
	d := &Device{raw: "spice:/tmp/vm.sock", openSession: func(string, string, string) (nativeSession, error) { return s, nil }, openEncoder: func(encodeOptions) (gpuEncoder, error) { return e, nil }}
	if err := d.Open(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d, s, e
}
func enqueue(s *fakeSession, change func(*dmabuf.Frame)) {
	f := dmabuf.Frame{FD: 7, Width: 640, Height: 480, Pitch: 2560, FourCC: dmabuf.XRGB8888, CropWidth: 640, CropHeight: 480, DeviceMajor: 226, DeviceMinor: 0}
	if change != nil {
		change(&f)
	}
	s.leases = append(s.leases, &frameLease{f, func() { s.ack++ }})
}
func TestGPUOnlyFrameAndIdleKeyframe(t *testing.T) {
	d, s, e := fixture(t)
	enqueue(s, func(f *dmabuf.Frame) { f.FlipY = true })
	f, err := d.Capture()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	if f.Format != capture.FormatH264 || f.Width != 640 || f.Height != 480 || e.frames != 1 || s.ack != 1 {
		t.Fatalf("invalid frame or lease: %+v ack=%d", f, s.ack)
	}
	if err = d.RequestKeyframe(); err != nil {
		t.Fatal(err)
	}
	repeated, err := d.Capture()
	if err != nil || repeated == nil {
		t.Fatalf("idle IDR: %v", err)
	}
	repeated.Release()
	if e.repeats != 1 || s.ack != 1 || d.Stats().GPUFrames != 1 || d.Stats().CPUReadbacks != 0 {
		t.Fatal("idle frame borrowed old upstream storage or used a CPU path")
	}
}
func TestLeaseAcknowledgedOnFailureAndPacing(t *testing.T) {
	for _, kind := range []string{"layout", "encoder open", "encode", "pacing"} {
		t.Run(kind, func(t *testing.T) {
			d, s, e := fixture(t)
			if kind == "layout" {
				enqueue(s, func(f *dmabuf.Frame) { f.Pitch = 1 })
			} else {
				enqueue(s, nil)
			}
			failure := errors.New("unsupported GPU")
			if kind == "encoder open" {
				d.openEncoder = func(encodeOptions) (gpuEncoder, error) { return nil, failure }
			}
			if kind == "encode" {
				e.err = failure
			}
			f, err := d.Capture()
			if f != nil {
				f.Release()
			}
			if kind != "pacing" && !errors.Is(err, capture.ErrStreamStopped) {
				t.Fatalf("expected terminal failure: %v", err)
			}
			if kind == "pacing" {
				d.lastFrame = time.Now()
				enqueue(s, nil)
				f, err = d.Capture()
				if f != nil || err != nil {
					t.Fatalf("pacing: %v", err)
				}
			}
			want := 1
			if kind == "pacing" {
				if d.pending == nil || s.ack != 1 {
					t.Fatal("paced frame was dropped instead of retained")
				}
				d.lastFrame = time.Now().Add(-time.Second)
				f, err = d.Capture()
				if err != nil || f == nil {
					t.Fatalf("last update was lost: %v", err)
				}
				f.Release()
				want = 2
			}
			if s.ack != want {
				t.Fatalf("ack=%d want=%d", s.ack, want)
			}
			if d.Stats().CPUReadbacks != 0 {
				t.Fatal("CPU fallback occurred")
			}
		})
	}
}
func TestCloseWaitsForGPUReadCompletion(t *testing.T) {
	d, s, e := fixture(t)
	enqueue(s, nil)
	e.inEncode = make(chan struct{})
	e.release = make(chan struct{})
	captured := make(chan struct{})
	go func() {
		f, _ := d.Capture()
		if f != nil {
			f.Release()
		}
		close(captured)
	}()
	<-e.inEncode
	closing := make(chan struct{})
	started := make(chan struct{})
	go func() { close(started); d.Close(); close(closing) }()
	<-started
	select {
	case <-closing:
		t.Fatal("closed live scanout before GPU completion")
	case <-time.After(20 * time.Millisecond):
	}
	close(e.release)
	<-captured
	<-closing
	if s.ack != 1 || !s.closed || !e.closeCalled {
		t.Fatal("lease/session/encoder cleanup missing")
	}
}
func TestTimeoutAndResize(t *testing.T) {
	d, s, _ := fixture(t)
	d.openedAt = time.Now().Add(-time.Minute)
	if _, err := d.Capture(); !errors.Is(err, capture.ErrStreamStopped) || !strings.Contains(err.Error(), "GL scanout") {
		t.Fatalf("timeout: %v", err)
	}
	d.openedAt = time.Now()
	enqueue(s, nil)
	f, err := d.Capture()
	if err != nil {
		t.Fatal(err)
	}
	f.Release()
	enqueue(s, func(f *dmabuf.Frame) { f.Width = 800; f.Pitch = 3200; f.CropWidth = 800 })
	f, err = d.Capture()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	if f.Width != 800 || s.ack != 2 {
		t.Fatal("resize was paced out or not acknowledged")
	}
}
func TestInputAndCloseReleaseHeldKeys(t *testing.T) {
	d, s, _ := fixture(t)
	if err := d.KeyDown(4); err != nil {
		t.Fatal(err)
	}
	if err := d.MouseButton(2, true); err != nil {
		t.Fatal(err)
	}
	d.Close()
	if s.inputs != 4 {
		t.Fatalf("held input not released: calls=%d", s.inputs)
	}
}
func TestPasswordEnvironmentIsResolvedOnEachOpen(t *testing.T) {
	d, s, _ := fixture(t)
	d.Close()
	var mu sync.Mutex
	var got []string
	d.opts.PasswordEnv = "ANYKVM_SPICE_TEST_PASSWORD"
	d.openSession = func(_, password, _ string) (nativeSession, error) {
		mu.Lock()
		got = append(got, password)
		mu.Unlock()
		return s, nil
	}
	for _, password := range []string{"first", "second"} {
		t.Setenv(d.opts.PasswordEnv, password)
		if err := d.Open(); err != nil {
			t.Fatal(err)
		}
		d.Close()
	}
	if strings.Join(got, ",") != "first,second" {
		t.Fatalf("password refresh: %v", got)
	}
}
func TestLocalSocketValidation(t *testing.T) {
	for _, raw := range []string{"spice:/tmp/vm.sock", "SPICE:/tmp/vm.sock"} {
		if err := ValidateSource(raw, Options{}); err != nil {
			t.Fatal(err)
		}
		path, err := socketPath(raw)
		if err != nil || path != "/tmp/vm.sock" {
			t.Fatalf("local path: %q, %v", path, err)
		}
	}
	for _, raw := range []string{"spice://host:5900", "spice://run/user/1000/vm.sock", "spice:///tmp/vm.sock", "spice:////tmp/vm.sock", "spice:relative", "spice:/tmp/a?password=secret", "spice:/tmp/%00sock", "spice:/tmp/a#fragment"} {
		if err := ValidateSource(raw, Options{}); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if err := ValidateSource("spice:/tmp/vm.sock", Options{Password: "x", PasswordEnv: "X"}); err == nil {
		t.Fatal("ambiguous credentials")
	}
	for _, raw := range []string{"vnc://host:5900", "spice+unix:/tmp/vm.sock", "spice+unix:///tmp/vm.sock"} {
		if dev, ok := NewCapture(raw, Options{}); dev != nil || ok {
			t.Fatalf("claimed unsupported source: %s", raw)
		}
	}
}
