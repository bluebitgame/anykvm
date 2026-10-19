package gamestream

import (
	"fmt"
	"testing"
)

func TestConfiguredBitrateBudget(t *testing.T) {
	base := "a=x-nv-video[0].clientViewportWd:1280\na=x-nv-video[0].clientViewportHt:720\na=x-nv-video[0].maxFPS:60\na=x-nv-vqos[0].bw.maximumBitrateKbps:10000\n"
	for _, tc := range []struct{ configured, want int }{
		{0, 10000}, {10000, 7372}, {1000, 605}, {100, 100},
	} {
		x := &streamSession{}
		if err := x.announce([]byte(base + fmt.Sprintf("a=x-ml-video.configuredBitrateKbps:%d\n", tc.configured))); err != nil {
			t.Fatal(err)
		}
		if x.options.Bitrate != tc.want {
			t.Fatalf("configured %d: video bitrate %d, want %d", tc.configured, x.options.Bitrate, tc.want)
		}
	}
	for _, value := range []string{"-1", "99", "150001"} {
		x := &streamSession{}
		if err := x.announce([]byte(base + "a=x-ml-video.configuredBitrateKbps:" + value + "\n")); err == nil {
			t.Fatalf("accepted invalid configured bitrate %s", value)
		}
	}
}
