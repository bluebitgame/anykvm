package capture

import (
	"image"
	"testing"
)

func TestScreenContentRectMatchesScaling(t *testing.T) {
	for _, tc := range []struct {
		name         string
		w, h, sw, sh int
		preserve     bool
		want         image.Rectangle
	}{
		{"matched native", 1650, 1050, 1650, 1050, true, image.Rect(0, 0, 1650, 1050)},
		{"fixed stretch", 1920, 1080, 2880, 1800, false, image.Rect(0, 0, 1920, 1080)},
		{"pillarbox", 1280, 720, 1000, 1000, true, image.Rect(280, 0, 1000, 720)},
		{"letterbox", 1650, 1050, 1920, 1080, true, image.Rect(0, 61, 1650, 989)},
		{"odd canvas", 1532, 1007, 1650, 1050, true, image.Rect(0, 16, 1532, 990)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := screenContentRect(tc.w, tc.h, tc.sw, tc.sh, tc.preserve); got != tc.want {
				t.Fatalf("content=%v want=%v", got, tc.want)
			}
		})
	}
}
