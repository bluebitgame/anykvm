package videoenc

import (
	"errors"
	"reflect"
	"testing"
)

func TestWindowsEncoderFallback(t *testing.T) {
	for _, tc := range []struct {
		name, codec, available string
		want                   []string
	}{
		{"vendor hardware first", "h264", "h264_amf", []string{"h264_nvenc", "h264_amf"}},
		{"missing FFmpeg", "h264", "mf-hardware", []string{"h264_nvenc", "h264_amf", "h264_qsv", "mf-hardware"}},
		{"system software before x264", "h264", "mf-software", []string{"h264_nvenc", "h264_amf", "h264_qsv", "mf-hardware", "mf-software"}},
		{"x264 last", "h264", "libx264", []string{"h264_nvenc", "h264_amf", "h264_qsv", "mf-hardware", "mf-software", "libx264"}},
		{"all fail", "h264", "", []string{"h264_nvenc", "h264_amf", "h264_qsv", "mf-hardware", "mf-software", "libx264"}},
		{"AV1 hardware", "av1", "mf-hardware", []string{"av1_nvenc", "av1_amf", "av1_qsv", "mf-hardware"}},
		{"AV1 has no software fallback", "av1", "", []string{"av1_nvenc", "av1_amf", "av1_qsv", "mf-hardware"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			unavailable := errors.New("backend unavailable")
			open := func(name string) (Encoder, error) {
				calls = append(calls, name)
				if name == tc.available {
					return &probeEncoder{}, nil
				}
				return nil, unavailable
			}
			e, err := selectWindowsEncoder(Options{Codec: tc.codec}, []string{tc.codec + "_nvenc", tc.codec + "_amf", tc.codec + "_qsv"},
				func(_ Options, name string) (Encoder, error) { return open(name) },
				func(_ Options, hardware bool) (Encoder, error) {
					if hardware {
						return open("mf-hardware")
					}
					return open("mf-software")
				}, func(_ Options, name, _ string) (Encoder, error) {
					if name != "libx264" {
						t.Fatalf("hardware encoder %s bypassed the vendor opener", name)
					}
					return open(name)
				})
			if !reflect.DeepEqual(calls, tc.want) || (e != nil) != (tc.available != "") {
				t.Fatalf("calls=%v encoder=%v error=%v", calls, e, err)
			}
			if tc.available == "" && !errors.Is(err, unavailable) {
				t.Fatalf("lost backend failures: %v", err)
			}
		})
	}
}

func TestWindowsDesktopAdapterSelection(t *testing.T) {
	for _, tc := range []struct {
		vendor uint32
		want   string
	}{
		{0x10de, "h264_nvenc"}, {0x1002, "h264_amf"}, {0x8086, "h264_qsv"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			// The matching output may be on a second card, with high LUID bits set.
			adapter := WindowsAdapter{LUID: 0xfedcba9876543210, VendorID: tc.vendor, Set: true}
			calls := 0
			e, err := selectWindowsEncoder(Options{Codec: "h264", WindowsAdapter: adapter}, []string{"h264_nvenc"},
				func(o Options, name string) (Encoder, error) {
					calls++
					if name != tc.want || o.WindowsAdapter != adapter {
						t.Fatalf("selected %s on %+v", name, o.WindowsAdapter)
					}
					return &probeEncoder{}, nil
				}, func(Options, bool) (Encoder, error) {
					t.Fatal("unexpected system fallback")
					return nil, ErrUnavailable
				},
				func(Options, string, string) (Encoder, error) {
					t.Fatal("unexpected library fallback")
					return nil, ErrUnavailable
				})
			if err != nil || e == nil || calls != 1 {
				t.Fatalf("encoder=%v calls=%d err=%v", e, calls, err)
			}
		})
	}
}

func TestWindowsDesktopFallbackIsExplicitlyUnbound(t *testing.T) {
	for _, vendor := range []uint32{0x1002, 0xffff} {
		var boundCalls, compatibilityCalls, systemCalls int
		e, err := selectWindowsEncoder(Options{Codec: "av1", WindowsAdapter: WindowsAdapter{Set: true, VendorID: vendor}}, []string{"av1_nvenc"},
			func(o Options, name string) (Encoder, error) {
				if o.WindowsAdapter.Set {
					boundCalls++
					if name != "av1_amf" {
						t.Fatalf("wrong bound candidate %s", name)
					}
				} else {
					compatibilityCalls++
					if o.WindowsAdapter != (WindowsAdapter{}) {
						t.Fatal("stale adapter identity on compatibility backend")
					}
				}
				return nil, ErrUnavailable
			}, func(o Options, hardware bool) (Encoder, error) {
				systemCalls++
				if o.WindowsAdapter.Set || !hardware {
					t.Fatal("invalid system fallback")
				}
				return &probeEncoder{}, nil
			}, func(Options, string, string) (Encoder, error) {
				t.Fatal("unexpected library fallback")
				return nil, ErrUnavailable
			})
		wantBound := 0
		if vendor == 0x1002 {
			wantBound = 1
		}
		if e == nil || err != nil || boundCalls != wantBound || compatibilityCalls != 1 || systemCalls != 1 {
			t.Fatalf("bound=%d compatibility=%d system=%d err=%v", boundCalls, compatibilityCalls, systemCalls, err)
		}
	}
}
