package spice

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"anykvm/internal/capture"
	"anykvm/internal/dmabuf"
	"anykvm/internal/framebuf"
)

type fakeSession struct {
	leases               []*frameLease
	ack, inputs          int
	nextCalls            int
	closed               bool
	err                  error
	audioQueue           []audioPacket
	audioOn, audioActive bool
	audioErr             error
	cursorState          *cursorSnapshot
}

func (s *fakeSession) next() (*frameLease, error) {
	s.nextCalls++
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

type fakePipeline struct {
	frames, repeats   int
	deferred          int
	err               error
	inEncode, release chan struct{}
	closeCalled       bool
}

func (e *fakePipeline) EncodeSource(f dmabuf.Frame, w, h int, at time.Time, pts int64) (*capture.Frame, error) {
	if e.deferred > 0 {
		e.deferred--
		return nil, nil
	}
	e.frames++
	if e.inEncode != nil {
		close(e.inEncode)
		<-e.release
	}
	if e.err != nil {
		return nil, e.err
	}
	return fakeVideoFrame(w, h, at), nil
}
func (e *fakePipeline) Repeat() (*capture.Frame, error) {
	e.repeats++
	if e.err != nil {
		return nil, e.err
	}
	return fakeVideoFrame(640, 480, time.Now()), nil
}
func (e *fakePipeline) Close() error { e.closeCalled = true; return nil }
func (e *fakePipeline) Stats() capture.DMABUFStats {
	return capture.DMABUFStats{GPUFrames: uint64(e.frames)}
}
func fakeVideoFrame(w, h int, at time.Time) *capture.Frame {
	b := framebuf.New([]byte{0, 0, 0, 1, 0x65}, nil)
	return &capture.Frame{Buffer: b, Data: b.Bytes(), Format: capture.FormatH264, Width: w, Height: h, SourceWidth: w, SourceHeight: h, CapturedAt: at}
}
func fixture(t *testing.T) (*Device, *fakeSession, *fakePipeline) {
	t.Helper()
	s, e := &fakeSession{}, &fakePipeline{}
	d := &Device{
		raw:         "spice:/tmp/vm.sock",
		openSession: func(string, string, string) (nativeSession, error) { return s, nil },
		newPipeline: func(*capture.DMABUFEncoder) (localPipeline, error) { return e, nil },
	}
	d.SetVideoCodec("h264")
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
	s.leases = append(s.leases, &frameLease{frame: f, done: func() { s.ack++ }})
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
	for _, kind := range []string{"layout", "pipeline creation", "encode", "pacing"} {
		t.Run(kind, func(t *testing.T) {
			d, s, e := fixture(t)
			if kind == "layout" {
				enqueue(s, func(f *dmabuf.Frame) { f.Pitch = 1 })
			} else {
				enqueue(s, nil)
			}
			failure := errors.New("unsupported GPU")
			if kind == "pipeline creation" {
				d.newPipeline = func(*capture.DMABUFEncoder) (localPipeline, error) { return nil, failure }
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
	for _, raw := range []string{"spice:///tmp/vm.sock", "spice:////tmp/vm.sock", "spice:relative", "spice:/tmp/a?password=secret", "spice:/tmp/%00sock", "spice:/tmp/a#fragment"} {
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

func TestLocalAcceptsViewerSelectedFormats(t *testing.T) {
	for _, codec := range []string{"", "mjpeg", "rawvideo", "h264", "hevc", "av1"} {
		t.Run(codec, func(t *testing.T) {
			d, s, _ := fixture(t)
			d.Close()
			s.closed = false
			d.SetVideoCodec(codec)
			d.SetDecodeImage(codec == "rawvideo")
			d.SetJPEGQuality(85)
			d.SetJPEGRegions(true)
			d.SetGPUSurface(codec == "rawvideo")
			if codec == "hevc" || codec == "av1" {
				d.SetVideoStreamOptions(&capture.VideoStreamOptions{Codec: codec, Width: 1280, Height: 720, FPS: 60, Bitrate: 16000})
			}
			if err := d.Open(); err != nil {
				t.Fatalf("format was rejected before shared GPU routing: %v", err)
			}
			if d.pipeline == nil || d.activePipeline != nil {
				t.Fatal("format setup did not use lazy shared GPU pipeline")
			}
			if _, direct := any(d).(capture.VideoStreamConfigurer); !direct {
				t.Fatal("local source lost direct encoding")
			}
		})
	}
}

func TestLocalRetainsLeaseWhenSharedPipelineDefers(t *testing.T) {
	for _, finish := range []string{"retry", "encode failure", "close"} {
		t.Run(finish, func(t *testing.T) {
			d, s, e := fixture(t)
			e.deferred = 1
			enqueue(s, nil)
			if frame, err := d.Capture(); frame != nil || err != nil {
				t.Fatalf("deferred capture: %+v %v", frame, err)
			}
			if d.pending == nil || s.ack != 0 || s.nextCalls != 1 {
				t.Fatal("pipeline backpressure released the draw or consumed another frame")
			}
			switch finish {
			case "close":
				d.Close()
				if !s.closed || !e.closeCalled {
					t.Fatal("close left deferred source or pipeline live")
				}
			case "encode failure":
				e.err = errors.New("GPU failed during retry")
				if frame, err := d.Capture(); frame != nil || !errors.Is(err, capture.ErrStreamStopped) {
					t.Fatalf("retry failure: %+v %v", frame, err)
				}
			default:
				frame, err := d.Capture()
				if err != nil || frame == nil {
					t.Fatalf("last deferred update lost: %v", err)
				}
				frame.Release()
			}
			if d.pending != nil || s.ack != 1 || s.nextCalls != 1 {
				t.Fatalf("draw ownership after %s: pending=%t ack=%d polls=%d", finish, d.pending != nil, s.ack, s.nextCalls)
			}
			d.Close()
			if s.ack != 1 {
				t.Fatal("close acknowledged the same draw twice")
			}
		})
	}
}
