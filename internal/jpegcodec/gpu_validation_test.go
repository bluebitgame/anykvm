package jpegcodec

import (
	"bytes"
	"image/jpeg"
	"os"
	"testing"
)

func TestGPUJPEGValidation(t *testing.T) {
	data, err := os.ReadFile("testdata/restart_base.jpeg")
	if err != nil {
		t.Fatal(err)
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateGPUJPEG(data, config.Width, config.Height); err != nil {
		t.Fatal(err)
	}
	if err := validateGPUJPEG(data, config.Width+1, config.Height); err == nil {
		t.Fatal("accepted wrong output dimensions")
	}
	// Keep a valid header and cut entropy: DecodeConfig alone misses this.
	cut := bytes.Index(data, []byte{0xff, 0xda})
	if cut < 0 {
		t.Fatal("fixture has no SOS")
	}
	cut += 2 + int(data[cut+2])<<8 + int(data[cut+3])
	if _, err := jpeg.DecodeConfig(bytes.NewReader(data[:cut])); err != nil {
		t.Fatal(err)
	}
	if err := validateGPUJPEG(data[:cut], config.Width, config.Height); err == nil {
		t.Fatal("accepted truncated entropy")
	}
}
