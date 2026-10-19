package rdp

import "testing"

func TestNegotiationUsesOnlyAllowedCodecs(t *testing.T) {
	for _, peer := range []struct {
		name             string
		version, flags   uint32
		avc, progressive bool
	}{
		{"avc", 0x80105, 0x10, true, true},
		{"bitmap", 0x80004, 2, false, true},
		{"thin", 0x80004, 3, false, false},
	} {
		for mask := 0; mask < 16; mask++ {
			allowed := CodecOptions{H264: mask&1 != 0, Progressive: mask&2 != 0, Planar: mask&4 != 0, ClearCodec: mask&8 != 0}
			var want displayCodec
			switch {
			case allowed.H264 && peer.avc:
				want = codecAVC420
			case allowed.Progressive && peer.progressive:
				want = codecProgressive
			case allowed.Planar:
				want = codecPlanar
			case allowed.ClearCodec:
				want = codecClear
			}
			p := join(u16(1), u32(peer.version), u32(4), u32(peer.flags))
			_, _, got, err := graphicsCapabilityWithReport(p, "auto", nil, &allowed)
			if want == 0 {
				if err == nil {
					t.Fatalf("%s mask=%d: selected forbidden codec %s", peer.name, mask, got)
				}
			} else if err != nil || got != want {
				t.Fatalf("%s mask=%d: got %s, %v; want %s", peer.name, mask, got, err, want)
			}
		}
	}
}
