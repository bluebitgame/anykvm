package capture

import (
	"anykvm/internal/jpegcodec"
	"anykvm/internal/logging"
	"bytes"
	"fmt"
	"log"
	"strings"
	"testing"
)

func TestEncodingLogOnlyChangesOnBackendTransition(t *testing.T) {
	var output bytes.Buffer
	logging.Configure(true, false)
	t.Cleanup(func() { logging.Configure(false, false) })
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	d := &ffmpegDevice{videoCodec: "mjpeg"}
	for _, gpu := range []bool{true, true, false, false, true, true} {
		native := &jpegTextureFixture{handled: gpu}
		frame, err := d.encodeMediaFrame(&Frame{Format: FormatRaw, Width: 16, Height: 16, nativeBuffer: native})
		if err != nil {
			t.Fatal(err)
		}
		frame.Release()
	}
	if strings.Count(output.String(), "capture: video codec=jpeg") != 3 || strings.Count(output.String(), `encoder="test GPU JPEG"`) != 2 || strings.Count(output.String(), fmt.Sprintf("encoder=%q", jpegcodec.Backend())) != 1 {
		t.Fatalf("encoder log duplicated or fallback missing: %s", output.String())
	}
}
