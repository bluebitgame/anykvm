package videoenc

import (
	"encoding/binary"
	"errors"
	"os"
	"runtime"
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
func TestAV1ProbeRequiresRecoveryFrame(t *testing.T) {
	data, err := os.ReadFile("../testdata/stream.ivf")
	if err != nil {
		t.Fatal(err)
	}
	key := data[44 : 44+int(binary.LittleEndian.Uint32(data[32:]))]
	for _, tc := range []struct {
		name string
		e    probeEncoder
		want bool
	}{
		{"key", probeEncoder{data: key}, true}, {"delayed key", probeEncoder{data: key, delayed: true}, true},
		{"no output", probeEncoder{}, false}, {"driver error", probeEncoder{fail: true}, false},
		{"wrong codec", probeEncoder{data: []byte{0, 0, 1, 0x65}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := probeAV1(func(o Options) (Encoder, error) {
				if o.Codec != "av1" {
					t.Fatal(o)
				}
				return &tc.e, nil
			})
			if got != tc.want || !tc.e.closed || tc.e.calls > 4 {
				t.Fatalf("got=%v encoder=%+v", got, tc.e)
			}
		})
	}
	if probeAV1(func(Options) (Encoder, error) { return nil, ErrUnavailable }) {
		t.Fatal("unavailable advertised")
	}
}
func TestDarwinAV1CannotFallBackToH264(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	if e, err := New(Options{Codec: "av1", Width: 1280, Height: 720}); err == nil {
		e.Close()
		t.Fatal("AV1 silently substituted")
	}
}
