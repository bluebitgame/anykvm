//go:build darwin && cgo

package jpegcodec

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestPixelBufferRows(t *testing.T) {
	const width, height = 17, 19
	pixels := colorFrame(width, height)
	want, err := EncodeBGRA(pixels, width, height, 80)
	if err != nil {
		t.Fatal(err)
	}
	for _, stride := range []int{width * 4, 128} {
		padded := bytes.Repeat([]byte{0xa5}, stride*(height-1)+width*4)
		for y := 0; y < height; y++ {
			copy(padded[y*stride:y*stride+width*4], pixels[y*width*4:(y+1)*width*4])
		}
		before := bytes.Clone(padded)
		buffer, err := encodePixelRows(padded, width, height, stride, 80)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(want, buffer.Bytes()) || !bytes.Equal(padded, before) {
			t.Fatal("pixel buffer encoding changed the input or included row padding")
		}
		// Neither the native input nor the packing scratch backs the result.
		clear(padded)
		next, err := encodePixelRows(padded, width, height, stride, 80)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(want, buffer.Bytes()) {
			t.Fatal("pixel buffer JPEG was overwritten by the next frame")
		}
		next.Release()
		buffer.Release()
	}
	if _, err := EncodePixelBufferOwned(nil, 80); err == nil {
		t.Fatal("accepted nil pixel buffer")
	}
}

func TestNativePixelBufferAccess(t *testing.T) {
	program := filepath.Join(t.TempDir(), "pixel-buffer")
	cmd := exec.Command("clang", "-fblocks", "-I.", "pixel_bgra_darwin.m", "testdata/pixel_bgra_darwin.m",
		"-framework", "Foundation", "-framework", "CoreVideo", "-framework", "CoreImage", "-framework", "CoreGraphics", "-o", program)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile pixel buffer fixture: %v\n%s", err, output)
	}
	if output, err := exec.Command(program).CombinedOutput(); err != nil {
		t.Fatalf("pixel buffer fixture: %v\n%s", err, output)
	}
}
