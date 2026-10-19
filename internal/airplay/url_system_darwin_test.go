//go:build darwin && cgo

package airplay

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"anykvm/internal/audiodec"
	"anykvm/internal/avmedia"
	"anykvm/internal/capture"
)

// Exercise the actual URL demux output with platform codecs, including B frames
// and ADTS-to-ASC conversion. FFmpeg does not decode any media in this test.
func TestURLMediaSystemDecode(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_AVMEDIA") == "" || os.Getenv("ANYKVM_TEST_SYSTEM_AIRPLAY") == "" {
		t.Skip("set ANYKVM_TEST_AVMEDIA and ANYKVM_TEST_SYSTEM_AIRPLAY")
	}
	server := httptest.NewServer(http.FileServer(http.Dir("../testdata/url")))
	defer server.Close()
	for _, name := range []string{"movie.mp4", "movie.m3u8", "fragmented.m3u8"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			source, err := avmedia.OpenMedia(ctx, server.URL+"/"+name)
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			video, err := newMirrorVideoDecoder()
			if err != nil {
				t.Fatal(err)
			}
			defer video.Close()
			if _, ok := video.(*systemVideoDecoder); !ok {
				t.Fatal("URL video did not select VideoToolbox")
			}
			var audio audiodec.Decoder
			defer func() {
				if audio != nil {
					audio.Close()
				}
			}()
			pictures, samples, nonzero := 0, 0, 0
			for {
				packet, err := source.Read()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if packet.Audio {
					if audio == nil {
						audio, err = audiodec.New(capture.AudioFrame{Codec: packet.Codec, Config: packet.Config, SampleRate: packet.SampleRate, Channels: packet.Channels, FramesPerPacket: 1024})
						if err != nil {
							t.Fatal(err)
						}
					}
					pcm, err := audio.Decode(packet.Data)
					if err != nil {
						t.Fatal(err)
					}
					samples += len(pcm) / 4
					for _, b := range pcm {
						if b != 0 {
							nonzero++
						}
					}
				} else {
					f, err := video.Decode(packet.Data)
					if err != nil {
						t.Fatal(err)
					}
					if f != nil {
						pictures++
						if f.Width != 64 || f.Height != 48 {
							t.Fatal("wrong video dimensions")
						}
					}
				}
				packet.Release()
			}
			if pictures != 20 || samples < 90000 || samples > 100000 || nonzero < 200000 {
				t.Fatalf("system output: pictures=%d samples=%d nonzero=%d", pictures, samples, nonzero)
			}
		})
	}
}
