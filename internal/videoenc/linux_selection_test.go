package videoenc

import (
	"errors"
	"reflect"
	"testing"
)

type linuxSelectionEncoder struct{}

func (*linuxSelectionEncoder) Encode([]byte, bool) ([]byte, error) { return nil, nil }
func (*linuxSelectionEncoder) Close()                              {}
func (*linuxSelectionEncoder) Name() string                        { return "test" }

func TestLinuxNativeSelectionWithoutFFmpeg(t *testing.T) {
	want := &linuxSelectionEncoder{}
	e, err := selectLinuxEncoder(Options{Codec: "h264"}, false, nil,
		func(Options) (Encoder, error) { return want, nil },
		func(Options, string, string) (Encoder, error) {
			t.Fatal("initialized FFmpeg despite a working native Pi encoder")
			return nil, ErrUnavailable
		})
	if err != nil || e != want {
		t.Fatalf("native selection: %v, %v", e, err)
	}
}

func TestLinuxEncoderFallbacks(t *testing.T) {
	for _, codec := range []string{"h264", "av1", "hevc"} {
		t.Run(codec, func(t *testing.T) {
			var calls []string
			_, err := selectLinuxEncoder(Options{Codec: codec}, true, []string{"renderD128"},
				func(Options) (Encoder, error) { calls = append(calls, "pi"); return nil, ErrUnavailable },
				func(_ Options, name, _ string) (Encoder, error) {
					calls = append(calls, name)
					return nil, ErrUnavailable
				})
			var want []string
			if codec == "h264" {
				want = []string{"h264_nvenc", "h264_vaapi", "pi", "libx264"}
			}
			if codec == "hevc" {
				want = []string{"hevc_nvenc", "hevc_vaapi"}
			}
			if codec == "av1" {
				want = []string{"av1_nvenc", "av1_vaapi"}
			}
			if !errors.Is(err, ErrUnavailable) || !reflect.DeepEqual(calls, want) {
				t.Fatalf("fallbacks = %v, %v; want %v", calls, err, want)
			}
		})
	}
}
