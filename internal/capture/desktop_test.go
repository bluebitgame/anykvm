package capture

import (
	"math"
	"testing"
)

func testDisplayMode(w, h int, hz float64) displayMode {
	return displayMode{width: w, height: h, pixelWidth: w, pixelHeight: h, refreshRate: hz}
}

func TestNearestDisplayMode(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request DesktopSize
		modes   []displayMode
		want    DesktopSize
	}{
		{"exact", DesktopSize{Width: 1920, Height: 1080}, []displayMode{testDisplayMode(2048, 1280, 60), testDisplayMode(1920, 1080, 60)}, DesktopSize{Width: 1920, Height: 1080}},
		{"wider client rejects insufficient height", DesktopSize{Width: 1920, Height: 1080}, []displayMode{testDisplayMode(1680, 1050, 60), testDisplayMode(2048, 1280, 60)}, DesktopSize{Width: 2048, Height: 1280}},
		{"wider client permits smaller width", DesktopSize{Width: 1920, Height: 1080}, []displayMode{testDisplayMode(1720, 1080, 60), testDisplayMode(2048, 1280, 60)}, DesktopSize{Width: 1720, Height: 1080}},
		{"height tie prioritizes width", DesktopSize{Width: 1920, Height: 1080}, []displayMode{testDisplayMode(1680, 1080, 60), testDisplayMode(1720, 1080, 60)}, DesktopSize{Width: 1720, Height: 1080}},
		{"narrower client prioritizes width", DesktopSize{Width: 1600, Height: 1200}, []displayMode{testDisplayMode(1600, 900, 60), testDisplayMode(1920, 1200, 60)}, DesktopSize{Width: 1600, Height: 900}},
		{"width tie prioritizes height", DesktopSize{Width: 1600, Height: 1200}, []displayMode{testDisplayMode(1600, 900, 60), testDisplayMode(1600, 1000, 60)}, DesktopSize{Width: 1600, Height: 1000}},
		{"narrower client rejects insufficient width", DesktopSize{Width: 1600, Height: 1200}, []displayMode{testDisplayMode(1536, 864, 60), testDisplayMode(1920, 1080, 60)}, DesktopSize{Width: 1920, Height: 1080}},
		{"portrait prioritizes width", DesktopSize{Width: 1080, Height: 1920}, []displayMode{testDisplayMode(1680, 1050, 60), testDisplayMode(2048, 1280, 60)}, DesktopSize{Width: 1680, Height: 1050}},
		{"equal aspect requires sufficient long edge", DesktopSize{Width: 1920, Height: 1080}, []displayMode{testDisplayMode(1600, 900, 60), testDisplayMode(2560, 1440, 60)}, DesktopSize{Width: 2560, Height: 1440}},
		{"equal portrait aspect requires sufficient height", DesktopSize{Width: 1080, Height: 1920}, []displayMode{testDisplayMode(810, 1440, 60), testDisplayMode(1440, 2560, 60)}, DesktopSize{Width: 1440, Height: 2560}},
		{"reported wide client chooses sufficient height", DesktopSize{Width: 1976, Height: 952, ScalePercent: 100}, []displayMode{testDisplayMode(1440, 900, 0), testDisplayMode(1650, 1050, 0)}, DesktopSize{Width: 1650, Height: 1050}},
		{"portrait exact", DesktopSize{Width: 1080, Height: 1920}, []displayMode{testDisplayMode(1920, 1080, 60), testDisplayMode(1080, 1920, 60)}, DesktopSize{Width: 1080, Height: 1920}},
		{"resolution before refresh", DesktopSize{Width: 1920, Height: 1080, RefreshRate: 120}, []displayMode{testDisplayMode(2048, 1280, 120), testDisplayMode(1920, 1080, 60)}, DesktopSize{Width: 1920, Height: 1080}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Ranking must not depend on the device's enumeration order.
			for i := 0; i < 2; i++ {
				got, ok := nearestDisplayMode(tc.modes, tc.request, displayMode{})
				if !ok || got.width != tc.want.Width || got.height != tc.want.Height {
					t.Fatalf("selected %dx%d, ok=%t; want %+v", got.width, got.height, ok, tc.want)
				}
				fit := math.Min(float64(tc.request.Width)/float64(got.pixelWidth), float64(tc.request.Height)/float64(got.pixelHeight))
				if fit > 1 {
					t.Fatalf("selected mode requires capture enlargement: scale=%f", fit)
				}
				tc.modes[0], tc.modes[1] = tc.modes[1], tc.modes[0]
			}
		})
	}
	if _, ok := nearestDisplayMode([]displayMode{{width: 0, height: 1080}, {width: 9000, height: 9000}}, DesktopSize{Width: 1920, Height: 1080}, displayMode{}); ok {
		t.Fatal("invalid display modes accepted")
	}
}

func TestDisplayModeRequiresSufficientComparisonEdge(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request DesktopSize
		mode    displayMode
	}{
		{"height", DesktopSize{Width: 1920, Height: 1080}, testDisplayMode(1680, 1050, 60)},
		{"width", DesktopSize{Width: 1600, Height: 1200}, testDisplayMode(1536, 864, 60)},
		{"equal landscape", DesktopSize{Width: 1920, Height: 1080}, testDisplayMode(1600, 900, 60)},
		{"equal portrait", DesktopSize{Width: 1080, Height: 1920}, testDisplayMode(810, 1440, 60)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if mode, ok := nearestDisplayMode([]displayMode{tc.mode}, tc.request, displayMode{}); ok {
				t.Fatalf("insufficient comparison edge accepted: %+v", mode)
			}
		})
	}
}

func TestDPIMatchRequiresSufficientBackingEdge(t *testing.T) {
	request := DesktopSize{Width: 2560, Height: 1440, ScalePercent: 200}
	// This mode has exactly the requested logical size, but too few pixels.
	one := testDisplayMode(1280, 720, 60)
	retina := testDisplayMode(1440, 900, 60)
	retina.pixelWidth, retina.pixelHeight = 2880, 1800
	for _, modes := range [][]displayMode{{one, retina}, {retina, one}} {
		mode, ok := nearestDisplayMode(modes, request, retina)
		if !ok || mode != retina {
			t.Fatalf("DPI matching selected insufficient backing pixels: %+v, ok=%t", mode, ok)
		}
	}
}

func TestDisplayRefreshAndRetinaTies(t *testing.T) {
	current := testDisplayMode(1440, 900, 60)
	current.pixelWidth, current.pixelHeight = 2880, 1800
	retina60 := testDisplayMode(960, 540, 60)
	retina60.pixelWidth, retina60.pixelHeight = 1920, 1080
	retina120 := retina60
	retina120.refreshRate = 120
	for _, hz := range []int{0, 60, 100, 120} {
		mode, ok := nearestDisplayMode([]displayMode{testDisplayMode(1920, 1080, 60), retina120, retina60, current}, DesktopSize{Width: 1920, Height: 1080, RefreshRate: hz}, current)
		wantHz := 60.
		if hz >= 100 {
			wantHz = 120
		}
		if !ok || mode.refreshRate != wantHz || mode.pixelWidth != 1920 || mode.width != 960 {
			t.Fatalf("request hz=%d: selected %+v", hz, mode)
		}
	}
}

func TestClientDesktopDPIModes(t *testing.T) {
	one := testDisplayMode(1920, 1080, 60)
	two := testDisplayMode(960, 540, 60)
	two.pixelWidth, two.pixelHeight = 1920, 1080
	fractional := testDisplayMode(1280, 720, 60)
	fractional.pixelWidth, fractional.pixelHeight = 2560, 1440
	quarter := testDisplayMode(1536, 864, 60)
	quarter.pixelWidth, quarter.pixelHeight = 3072, 1728
	for _, tc := range []struct{ percent, wantWidth int }{
		{0, 960}, {100, 1920}, {125, 1536}, {150, 1280}, {200, 960},
	} {
		// Even exact backing-pixel matches must give way to requested UI size.
		mode, ok := nearestDisplayMode([]displayMode{one, two, testDisplayMode(1280, 720, 60), fractional, quarter}, DesktopSize{Width: 1920, Height: 1080, ScalePercent: tc.percent}, two)
		if !ok || mode.width != tc.wantWidth {
			t.Fatalf("scale=%d: selected=%+v", tc.percent, mode)
		}
		if tc.percent == 150 && mode.pixelWidth != 2560 {
			t.Fatal("DPI match enlarged a low-resolution source despite an available HiDPI mode")
		}
	}
	if _, ok := nearestDisplayMode([]displayMode{one}, DesktopSize{Width: 1920, Height: 1080, ScalePercent: 501}, one); ok {
		t.Fatal("invalid desktop DPI accepted")
	}
}

func TestLowResolutionScaleProtection(t *testing.T) {
	for _, tc := range []struct{ width, height, want int }{
		{1280, 720, 100}, {1680, 1050, 100}, {720, 1280, 200},
		{2560, 900, 100}, {1600, 1200, 200}, {1920, 1079, 100},
		{1919, 1080, 200}, {1920, 1080, 200}, {1080, 1920, 200}, {2560, 1440, 200},
	} {
		request := DesktopSize{Width: tc.width, Height: tc.height, RefreshRate: 60, ScalePercent: 200}
		protected := request.WithScaleProtection()
		if protected.ScalePercent != tc.want || protected.Width != request.Width || protected.Height != request.Height || protected.RefreshRate != 60 {
			t.Fatalf("request=%+v protected=%+v", request, protected)
		}
		low := testDisplayMode(tc.width/2, tc.height/2, 60)
		low.pixelWidth, low.pixelHeight = tc.width, tc.height
		mode, ok := nearestDisplayMode([]displayMode{low, testDisplayMode(tc.width, tc.height, 60)}, request, low)
		if !ok {
			t.Fatal("no scale-protected mode")
		}
		if tc.want == 100 && mode.width != tc.width {
			t.Fatalf("sub-1080p request retained HiDPI UI scaling: %+v", mode)
		}
		if tc.want == 200 && mode.width != tc.width/2 {
			t.Fatalf("Full HD DPI request was overridden: %+v", mode)
		}
	}
	if got := (DesktopSize{Width: 1280, Height: 720}).WithScaleProtection(); got.ScalePercent != 100 {
		t.Fatal("omitted DPI did not receive low-resolution protection")
	}
}

func TestLowResolutionRequiresNativeScaleOne(t *testing.T) {
	retina := testDisplayMode(1440, 900, 60)
	retina.pixelWidth, retina.pixelHeight = 2880, 1800
	one := testDisplayMode(1650, 1050, 60)
	small := testDisplayMode(1440, 900, 60)
	request := DesktopSize{Width: 1532, Height: 1007, ScalePercent: 200}
	for _, modes := range [][]displayMode{{retina, small, one}, {one, small, retina}} {
		mode, ok := nearestDisplayMode(modes, request, retina)
		if !ok || mode != one {
			t.Fatalf("1532x1007 retained native HiDPI despite low-resolution protection: %+v", mode)
		}
	}
	if _, ok := nearestDisplayMode([]displayMode{retina}, request, retina); ok {
		t.Fatal("a Retina-only display silently bypassed the 1x protection")
	}
	if _, ok := nearestDisplayMode([]displayMode{small}, request, retina); ok {
		t.Fatal("low-resolution protection accepted a 1x mode with an insufficient comparison edge")
	}
}
