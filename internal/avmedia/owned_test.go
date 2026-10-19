package avmedia

import (
	"bytes"
	"os"
	"testing"
)

func TestOwnedLibraryOutputOutlivesSessions(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_AVMEDIA") == "" {
		t.Skip("set ANYKVM_TEST_AVMEDIA and ANYKVM_FFMPEG_LIBDIR")
	}
	source, err := OpenSource("lavfi", "testsrc2=size=64x48:rate=30", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	first, err := source.ReadOwned()
	if err != nil || first == nil || first.Buffer == nil {
		t.Fatalf("owned source: %v", err)
	}
	defer first.Release()
	expected := append([]byte(nil), first.Data...)
	for range 3 {
		next, err := source.ReadOwned()
		if err != nil {
			t.Fatal(err)
		}
		next.Release()
	}
	source.Close()
	if !bytes.Equal(first.Data, expected) {
		t.Fatal("source output invalidated by read/close")
	}
	encoder, err := NewEncoder(VideoOptions{Width: 64, Height: 48, FPS: 30, Bitrate: 1000, Codec: "mjpeg"})
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	jpeg, err := encoder.EncodeOwned(first.Data, 64, 48, "bgra", false)
	if err != nil || jpeg == nil {
		t.Fatalf("owned encode: %v", err)
	}
	defer jpeg.Release()
	snapshot := append([]byte(nil), jpeg.Bytes()...)
	for range 3 {
		next, err := encoder.EncodeOwned(first.Data, 64, 48, "bgra", false)
		if err != nil {
			t.Fatal(err)
		}
		next.Release()
	}
	encoder.Close()
	if !bytes.Equal(jpeg.Bytes(), snapshot) {
		t.Fatal("encoder output invalidated by read/close")
	}
	decoder, err := NewDecoder("mjpeg")
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	decoded, err := decoder.Decode(jpeg.Bytes())
	if err != nil || decoded == nil || len(decoded.Data) != 64*48*4 {
		t.Fatalf("borrowed native output decode: %v", err)
	}
}
