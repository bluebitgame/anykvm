//go:build windows && cgo

package capture

import (
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

func TestWindowsWASAPIAudioRouting(t *testing.T) {
	for _, selector := range []string{"wasapi", "wasapi:Speakers (USB Audio)", "wasapi:id:{endpoint}"} {
		if _, ok := NewAudioDevice(selector).(*wasapiAudioDevice); !ok {
			t.Fatalf("selector %q did not select WASAPI", selector)
		}
	}
	if _, ok := NewAudioDevice("Microphone").(*windowsAudioDevice); !ok {
		t.Fatal("loopback replaced existing microphone input")
	}
}

// Run while a game/browser/media player is producing sound on the selected
// output. This verifies engine conversion, real capture, reopen and cancellation.
func TestWindowsWASAPILoopback(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_WASAPI") == "" {
		t.Skip("set ANYKVM_TEST_WASAPI=1 on Windows and play audio on the selected output")
	}
	selector := os.Getenv("ANYKVM_TEST_WASAPI_DEVICE")
	d := newWASAPIAudioDevice(selector).(*wasapiAudioDevice)
	for run := 0; run < 2; run++ {
		if err := d.Open(); err != nil {
			t.Fatal(err)
		}
		if err := d.Open(); err == nil {
			d.Close()
			t.Fatal("double open accepted")
		}
		deadline := time.Now().Add(10 * time.Second)
		heard := false
		for time.Now().Before(deadline) {
			f, err := d.Capture()
			if err != nil {
				d.Close()
				t.Fatal(err)
			}
			if f != nil {
				if f.SampleRate != 48000 || f.Channels != 2 || len(f.PCM)%4 != 0 || f.Epoch == 0 {
					d.Close()
					t.Fatal("invalid loopback PCM format")
				}
				for _, b := range f.PCM {
					if b != 0 {
						heard = true
						break
					}
				}
			}
			if heard {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		if !heard {
			d.Close()
			t.Fatal("no audible loopback samples; play sound on the selected playback endpoint")
		}
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				d.Capture()
			}
		}()
		closed := make(chan struct{})
		go func() { d.Close(); close(closed) }()
		select {
		case <-closed:
		case <-time.After(time.Second):
			t.Fatal("loopback shutdown did not cancel capture")
		}
		wg.Wait()
		if _, err := d.Capture(); !errors.Is(err, ErrStreamStopped) {
			t.Fatalf("closed capture error=%v", err)
		}
	}
}
