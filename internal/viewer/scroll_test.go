//go:build viewer

package viewer

import (
	"runtime"
	"testing"
)

func TestWheelInvertForMode(t *testing.T) {
	if wheelInvertForMode(ScrollInvert) != true {
		t.Fatal("invert should always invert")
	}
	if wheelInvertForMode(ScrollNone) || wheelInvertForMode(ScrollTraditional) {
		t.Fatal("none/traditional should not invert")
	}
	if runtime.GOOS != "darwin" && wheelInvertForMode(ScrollNatural) {
		t.Fatal("natural should not invert off darwin")
	}
}

func TestWheelDeltaFromSDL_nonDarwin(t *testing.T) {
	if wheelInvertForMode(ScrollNatural) {
		t.Skip("darwin natural scrolling active in this environment")
	}
	if d := wheelDeltaFromSDL(0, false); d != 0 {
		t.Fatalf("zero: got %d", d)
	}
	if d := wheelDeltaFromSDL(3, false); d != 3 {
		t.Fatalf("none: got %d", d)
	}
	if d := wheelDeltaFromSDL(3, true); d != -3 {
		t.Fatalf("invert: got %d", d)
	}
	if d := wheelDeltaFromSDL(-2, true); d != 2 {
		t.Fatalf("invert neg: got %d", d)
	}
	if d := wheelDeltaFromSDL(200, false); d != 127 {
		t.Fatalf("clamp high: got %d", d)
	}
	if d := wheelDeltaFromSDL(-200, false); d != -128 {
		t.Fatalf("clamp low: got %d", d)
	}
}
