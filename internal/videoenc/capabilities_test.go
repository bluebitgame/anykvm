package videoenc

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

type probeEncoder struct {
	data    []byte
	calls   int
	closed  bool
	fail    bool
	delayed bool
}

func (e *probeEncoder) Encode(_ []byte, force bool) ([]byte, error) {
	e.calls++
	if e.fail {
		return nil, errors.New("driver failure")
	}
	if e.delayed && e.calls < 2 {
		return nil, nil
	}
	return e.data, nil
}
func (e *probeEncoder) Name() string { return "test" }
func (e *probeEncoder) Close()       { e.closed = true }
func TestDarwinAV1CannotFallBackToH264(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	if e, err := New(Options{Codec: "av1", Width: 1280, Height: 720}); err == nil {
		e.Close()
		t.Fatal("AV1 silently substituted")
	}
}

func TestCodecCapabilityQueriesOnlyRequestedCodecOnce(t *testing.T) {
	for _, codec := range []string{"hevc", "av1"} {
		for _, supported := range []bool{false, true} {
			var capability codecCapability
			var calls atomic.Int32
			query := func(request string) bool {
				if request != codec {
					t.Errorf("queried %s instead of %s", request, codec)
				}
				calls.Add(1)
				return supported
			}
			var wg sync.WaitGroup
			for i := 0; i < 16; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if capability.get(codec, query) != supported {
						t.Error("driver declaration changed")
					}
				}()
			}
			wg.Wait()
			if calls.Load() != 1 {
				t.Fatalf("capability queried %d times", calls.Load())
			}
		}
	}
}
