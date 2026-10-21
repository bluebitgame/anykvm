package vnc

import (
	"anykvm/internal/capture"
	"net"
	"testing"
)

func TestOptionalCodecPolicy(t *testing.T) {
	for mask := 0; mask < 8; mask++ {
		conn, peer := net.Pipe()
		defer conn.Close()
		defer peer.Close()
		p := CodecOptions{JPEG: mask&1 != 0, Tight: mask&2 != 0, H264: mask&4 != 0}
		c := &client{codecs: &p, encodings: []int32{encJPEG, encTight, encH264, encHextile, encZlib, encZRLE, encCursor, encJPEGQualityHigh}, pixelFmt: defaultPixelFormat()}
		c.conn = conn
		var pixels, h264 bool
		c.server = NewServer(Config{
			OnPixelNeedChanged: func(need bool) { pixels = need },
			OnVideoH264Changed: func(want bool) { h264 = want },
		})
		c.refreshPixelNeed()
		c.refreshVideoH264Want()
		if pixels != (!p.JPEG && !p.Tight) || h264 != p.H264 {
			t.Fatalf("mask %d: capture pixel need=%v, H264=%v", mask, pixels, h264)
		}
		for enc, want := range map[int32]bool{encJPEG: p.JPEG, encTight: p.Tight, encH264: p.H264, encHextile: true, encZlib: true, encZRLE: true, encCursor: true} {
			if got := c.hasEncoding(enc); got != want {
				t.Fatalf("mask %d encoding %d: %v", mask, enc, got)
			}
		}
		want := int32(encZRLE)
		if p.Tight {
			want = encTight
		} else if p.JPEG {
			want = encJPEG
		}
		for _, format := range []capture.VideoFormat{capture.FormatRaw, capture.FormatJPEG} {
			if got := c.chooseEncoding(defaultPixelFormat(), &frameData{format: format}); got != want {
				t.Fatalf("mask %d format %v: selected %d, want %d", mask, format, got, want)
			}
		}
		c.encodings = nil
		if got := c.chooseEncoding(defaultPixelFormat(), &frameData{format: capture.FormatRaw}); got != encRaw {
			t.Fatalf("Raw fallback disabled: %d", got)
		}
	}
}

func TestCodecPolicySnapshot(t *testing.T) {
	p := DefaultCodecOptions()
	s := NewServer(Config{Codecs: &p})
	p.H264 = false
	if !s.config.Codecs.H264 {
		t.Fatal("server did not snapshot policy")
	}
	old := s.config.Codecs
	s.UpdateCodecs(CodecOptions{})
	if !old.H264 || s.config.Codecs.H264 {
		t.Fatal("update changed existing client policy")
	}
}
