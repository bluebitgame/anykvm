package capture

import "testing"

func TestParseFPSFromLog(t *testing.T) {
	cases := []struct {
		name string
		log  string
		want int
	}{
		{
			name: "input stream fps preferred, progress ignored",
			log: "  Stream #0:0: Video: rawvideo, uyvy422, 1280x720, 60 fps, 60 tbr, 1000k tbn\n" +
				"  Stream #0:0 -> #0:0\n" +
				"  Stream #0:0: Video: mjpeg, yuv444p, 1280x720, q=2-31, 30 fps, 30 tbn\n" +
				"frame=    1 fps=0.0 q=5.0 Lsize=36KiB\n",
			want: 60,
		},
		{
			name: "fractional rounds",
			log:  "Stream #0:0: Video: h264, 1920x1080, 29.97 fps, 30 tbr\n",
			want: 30,
		},
		{
			name: "tbr fallback when no fps",
			log:  "Stream #0:0: Video: rawvideo, 640x480, 50 tbr, 1000k tbn\n",
			want: 50,
		},
		{
			name: "unknown",
			log:  "some unrelated ffmpeg output\nframe= 10 fps=0.0\n",
			want: 0,
		},
	}
	for _, c := range cases {
		if got := parseFPSFromLog(c.log); got != c.want {
			t.Errorf("%s: parseFPSFromLog = %d, want %d", c.name, got, c.want)
		}
	}
}
