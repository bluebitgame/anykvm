package spice

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"anykvm/internal/capture"
)

// A blocked local encode must not block the other session's bitmap/media reads
// or teardown. Even the local audio reader must remain independent of the GPU.
func TestLocalEncodeAndRemoteTeardownAreIndependent(t *testing.T) {
	local, localSession, encoder := fixture(t)
	remote, remoteSession := remoteFixture(t)
	localAudio, remoteAudio := local.AudioDevice(), remote.AudioDevice()
	for _, audio := range []capture.AudioDevice{localAudio, remoteAudio} {
		if err := audio.Open(); err != nil {
			t.Fatal(err)
		}
		defer audio.Close()
	}
	localSession.audioActive, remoteSession.audioActive = true, true
	localSession.audioQueue = []audioPacket{packet(111, -111)}
	remoteSession.audioQueue = []audioPacket{packet(222, -222)}
	remoteSession.cursorState = &cursorSnapshot{known: true, visible: true, systemDefault: true}
	enqueueBitmap(remoteSession, 1, 1, []byte{1, 2, 3, 255})
	enqueue(localSession, nil)

	encoder.inEncode, encoder.release = make(chan struct{}), make(chan struct{})
	localResult := make(chan error, 1)
	// Always release and join before fixture cleanup, including on failure.
	defer func() {
		close(encoder.release)
		if err := <-localResult; err != nil {
			t.Errorf("local encode after remote teardown: %v", err)
		}
	}()
	go func() {
		frame, err := local.Capture()
		if frame != nil {
			frame.Release()
		}
		localResult <- err
	}()
	<-encoder.inEncode

	localPCM := make(chan *capture.AudioFrame, 1)
	go func() { frame, _ := localAudio.Capture(); localPCM <- frame }()
	select {
	case frame := <-localPCM:
		if frame == nil || !bytes.Equal(frame.PCM, pcm(111, -111)) {
			t.Fatal("local audio lost or crossed sessions")
		}
	case <-time.After(time.Second):
		t.Fatal("local audio waits for GPU encoding")
	}

	remoteResult := make(chan error, 1)
	go func() {
		frame, err := remote.Capture()
		if err == nil && (frame == nil || frame.Format != capture.FormatRaw) {
			err = errors.New("missing remote bitmap")
		}
		if frame != nil {
			frame.Release()
		}
		audio, audioErr := remoteAudio.Capture()
		if err == nil {
			err = audioErr
		}
		if err == nil && (audio == nil || !bytes.Equal(audio.PCM, pcm(222, -222))) {
			err = errors.New("remote audio crossed sessions")
		}
		cursor, cursorErr := remote.CaptureCursor()
		if err == nil {
			err = cursorErr
		}
		if err == nil && (cursor == nil || !cursor.SystemDefault) {
			err = errors.New("remote cursor lost")
		}
		remote.Close()
		remoteResult <- err
	}()
	select {
	case err := <-remoteResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("remote capture or teardown waits for the other GPU session")
	}
	if localSession.closed || localSession.ack != 0 {
		t.Fatal("remote teardown released the local draw or session")
	}
	if !remoteSession.closed || remote.Stats().BitmapFrames != 1 || remote.Stats().GPUFrames != 0 {
		t.Fatal("remote source used or closed the wrong pipeline")
	}
}

func TestLocalAndRemoteFailureReopenIsolation(t *testing.T) {
	for _, failedMode := range []string{"local", "remote"} {
		t.Run(failedMode, func(t *testing.T) {
			local, localSession, _ := fixture(t)
			remote, remoteSession := remoteFixture(t)
			type source struct {
				device  capture.Device
				core    *Device
				session *fakeSession
			}
			sources := map[string]*source{
				"local":  {device: local, core: local, session: localSession},
				"remote": {device: remote, core: remote.source, session: remoteSession},
			}
			expectedFormat := func(name string) capture.VideoFormat {
				if name == "local" {
					return capture.FormatH264
				}
				return capture.FormatRaw
			}
			enqueueFrame := func(name string, s *fakeSession) {
				if name == "local" {
					enqueue(s, nil)
				} else {
					enqueueBitmap(s, 1, 1, []byte{9, 8, 7, 255})
				}
			}
			frames := make(map[string]*capture.Frame)
			audios := make(map[string]capture.AudioDevice)
			for name, s := range sources {
				if err := s.core.KeyDown(4); err != nil {
					t.Fatal(err)
				}
				audios[name] = s.core.AudioDevice()
				if err := audios[name].Open(); err != nil {
					t.Fatal(err)
				}
				defer audios[name].Close()
				s.session.audioActive = true
				s.session.cursorState = &cursorSnapshot{known: true, visible: true, systemDefault: true}
				enqueueFrame(name, s.session)
				frame, err := s.device.Capture()
				if err != nil || frame == nil || frame.Format != expectedFormat(name) {
					t.Fatalf("%s initial frame: %+v %v", name, frame, err)
				}
				frames[name] = frame
				defer frame.Release()
			}
			failed := sources[failedMode]
			liveMode := "remote"
			if failedMode == "remote" {
				liveMode = "local"
			}
			live := sources[liveMode]
			savedFrame := bytes.Clone(frames[liveMode].Data)
			generation := live.core.generation
			failed.session.err = errors.New("connection lost")
			failed.core.RequestKeyframe()
			if _, err := failed.device.Capture(); !errors.Is(err, capture.ErrStreamStopped) {
				t.Fatalf("%s failure: %v", failedMode, err)
			}
			failed.device.Close()
			if live.session.closed || live.session.inputs != 1 || live.core.generation != generation {
				t.Fatal("failure closed or reset the other source")
			}

			// Reopen only the failed source; the other source and its consumer
			// retain their session, outstanding frame and media state.
			replacement := &fakeSession{audioActive: true}
			failed.core.openSession = func(address, password, node string) (nativeSession, error) {
				if (node == "") != (failedMode == "remote") {
					t.Fatalf("%s reopened through the wrong transport", failedMode)
				}
				return replacement, nil
			}
			failed.core.newPipeline = func(*capture.DMABUFEncoder) (localPipeline, error) {
				if failedMode == "remote" {
					t.Fatal("remote reopen attempted local GPU encoding")
				}
				return &fakePipeline{}, nil
			}
			if err := failed.device.Open(); err != nil {
				t.Fatal(err)
			}
			if !replacement.audioOn {
				t.Fatal("open audio consumer did not follow its reopened source")
			}
			enqueueFrame(failedMode, replacement)
			reopened, err := failed.device.Capture()
			if err != nil || reopened == nil || reopened.Format != expectedFormat(failedMode) {
				t.Fatalf("%s reopen: %+v %v", failedMode, reopened, err)
			}
			reopened.Release()

			live.session.audioQueue = []audioPacket{packet(333, -333)}
			audio, err := audios[liveMode].Capture()
			if err != nil || audio == nil || !bytes.Equal(audio.PCM, pcm(333, -333)) {
				t.Fatalf("surviving audio: %+v %v", audio, err)
			}
			cursor, err := live.core.CaptureCursor()
			if err != nil || cursor == nil || !cursor.SystemDefault {
				t.Fatalf("surviving cursor: %+v %v", cursor, err)
			}
			live.core.RequestKeyframe()
			repeated, err := live.device.Capture()
			if err != nil || repeated == nil || repeated.Format != expectedFormat(liveMode) {
				t.Fatalf("surviving idle frame: %+v %v", repeated, err)
			}
			repeated.Release()
			if !bytes.Equal(frames[liveMode].Data, savedFrame) || live.session.closed || live.core.generation != generation {
				t.Fatal("reopen mutated the other source's outstanding frame or session")
			}
			if local.Stats().BitmapFrames != 0 || remote.Stats().GPUFrames != 0 {
				t.Fatal("video pipelines crossed after failure/reopen")
			}
		})
	}
}

// The session adapter must not turn a malformed/crossed payload into a silent
// mode switch, especially a CPU-frame fallback for the local GPU-only source.
func TestLocalAndRemoteRejectOtherModePayload(t *testing.T) {
	t.Run("local-rejects-bitmap", func(t *testing.T) {
		local, s, encoder := fixture(t)
		enqueueBitmap(s, 1, 1, []byte{1, 2, 3, 255})
		if _, err := local.Capture(); !errors.Is(err, capture.ErrStreamStopped) {
			t.Fatalf("local accepted remote pixels: %v", err)
		}
		if s.ack != 1 || encoder.frames != 0 || local.Stats().BitmapFrames != 0 {
			t.Fatal("local crossed into the bitmap path or lost lease cleanup")
		}
	})
	t.Run("remote-rejects-dmabuf", func(t *testing.T) {
		remote, s := remoteFixture(t)
		remote.source.newPipeline = func(*capture.DMABUFEncoder) (localPipeline, error) {
			t.Fatal("remote attempted DMA-BUF hardware encoding")
			return nil, nil
		}
		enqueue(s, nil)
		if _, err := remote.Capture(); !errors.Is(err, capture.ErrStreamStopped) {
			t.Fatalf("remote accepted a GPU lease: %v", err)
		}
		if s.ack != 1 || remote.Stats().GPUFrames != 0 {
			t.Fatal("remote crossed into the GPU path or lost lease cleanup")
		}
	})
}
