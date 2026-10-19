package capture

import (
	"runtime"
	"testing"
)

func TestScreenCursorInputOption(t *testing.T) {
	name := "draw_mouse"
	if runtime.GOOS == "darwin" {
		name = "capture_cursor"
	}
	for _, hide := range []bool{false, true} {
		args := buildScreenFFmpegArgs(0, 1280, 720, 30, hide)
		_, _, options, err := mediaInputArgs(args)
		if err != nil {
			t.Fatal(err)
		}
		want := "1"
		if hide {
			want = "0"
		}
		if options[name] != want {
			t.Fatalf("hide=%v: input options %v", hide, options)
		}
	}
}
