//go:build ffmpeg && cgo

package avmedia

/*
#cgo CFLAGS: -I${SRCDIR}/third_party
#cgo linux LDFLAGS: -ldl
#include "native.h"
#include <stdlib.h>
*/
import "C"
import (
	"anykvm/internal/framebuf"
	"anykvm/internal/framerate"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"unsafe"
)

func loadLibraries(dirs []string) error {
	var msg [512]C.char
	var failures []string
	for _, dir := range dirs {
		p := C.CString(dir)
		result := C.bm_load(p, &msg[0], 512)
		C.free(unsafe.Pointer(p))
		if result == 0 {
			return nil
		}
		failures = append(failures, C.GoString(&msg[0]))
	}
	return fmt.Errorf("%w: %s", ErrUnavailable, strings.Join(failures, "; "))
}

type Session struct {
	mu          sync.Mutex // serialize codec operations
	life        sync.Mutex // allow interrupt while a Read holds mu
	ptr         *C.bm_session
	thread      *mediaThread
	sourceCodec string
	sourceRate  framerate.Rate
}

func resultSession(p *C.bm_session, msg *C.char) (*Session, error) {
	if p == nil {
		return nil, fmt.Errorf("libav: %s", C.GoString(msg))
	}
	return &Session{ptr: p}, nil
}

// HasEncoder checks the library declaration without opening an encoder.
func HasEncoder(name string) bool {
	if Available() != nil {
		return false
	}
	p := C.CString(name)
	defer C.free(unsafe.Pointer(p))
	return C.bm_has_encoder(p) != 0
}

func NewEncoder(o VideoOptions) (*Session, error) {
	o.Rate = o.Rate.Or(o.FPS)
	if err := Available(); err != nil {
		return nil, err
	}
	if o.Width < 2 || o.Height < 2 || o.Width > 8192 || o.Height > 8192 || o.Width%2 != 0 || o.Height%2 != 0 || !o.Rate.Valid() || o.Rate.Num < o.Rate.Den || int64(o.Rate.Num) > 240*int64(o.Rate.Den) || o.Bitrate < 0 || o.Bitrate > 1000000 {
		return nil, fmt.Errorf("invalid video encoder options")
	}
	name, device := C.CString(o.Codec), C.CString(o.Device)
	defer C.free(unsafe.Pointer(name))
	defer C.free(unsafe.Pointer(device))
	var msg [512]C.char
	p := C.bm_encoder(name, device, C.int(o.Width), C.int(o.Height), C.int(o.Rate.Num), C.int(o.Rate.Den), C.int(o.Bitrate), 0, &msg[0], 512)
	return resultSession(p, &msg[0])
}

// NewJPEGEncoder preserves source dimensions, including odd and single-pixel edges.
func NewJPEGEncoder(width, height, quality int) (*Session, error) {
	if width < 1 || height < 1 || width > 8192 || height > 8192 || quality < 1 || quality > 100 {
		return nil, fmt.Errorf("invalid JPEG encoder options")
	}
	if err := Available(); err != nil {
		return nil, err
	}
	var msg [512]C.char
	p := C.bm_jpeg_encoder(C.int(width), C.int(height), C.int(quality), &msg[0], 512)
	return resultSession(p, &msg[0])
}

func NewAudioEncoder(duration, bitrate int) (*Session, error) {
	if err := Available(); err != nil {
		return nil, err
	}
	if duration != 5 && duration != 10 {
		return nil, fmt.Errorf("invalid Opus duration")
	}
	name, device := C.CString("libopus"), C.CString("")
	defer C.free(unsafe.Pointer(name))
	defer C.free(unsafe.Pointer(device))
	var msg [512]C.char
	p := C.bm_encoder(name, device, 0, 0, 0, 1, C.int(bitrate), C.int(duration), &msg[0], 512)
	return resultSession(p, &msg[0])
}
func NewDecoder(codec string) (*Session, error) {
	if err := Available(); err != nil {
		return nil, err
	}
	name := C.CString(codec)
	defer C.free(unsafe.Pointer(name))
	var msg [512]C.char
	p := C.bm_decoder(name, &msg[0], 512)
	return resultSession(p, &msg[0])
}

// NewAudioDecoder decodes raw packets using codec configuration, and converts
// output to interleaved 48 kHz stereo S16LE, like audio capture sources.
func NewAudioDecoder(codec string, config []byte, sampleRate, channels int) (*Session, error) {
	if len(config) > 4096 || sampleRate < 8000 || sampleRate > 192000 || channels < 1 || channels > 8 {
		return nil, fmt.Errorf("invalid audio decoder options")
	}
	if err := Available(); err != nil {
		return nil, err
	}
	name := C.CString(codec)
	defer C.free(unsafe.Pointer(name))
	var extra *C.uint8_t
	if len(config) != 0 {
		extra = (*C.uint8_t)(unsafe.Pointer(&config[0]))
	}
	var msg [512]C.char
	p := C.bm_audio_decoder(name, extra, C.size_t(len(config)), C.int(sampleRate), C.int(channels), &msg[0], 512)
	runtime.KeepAlive(config)
	return resultSession(p, &msg[0])
}
func OpenSource(format, url string, options map[string]string, audio bool) (*Session, error) {
	return openSource(format, url, options, audio, "")
}

// OpenVideoSource preserves matching compressed video before considering decode.
func OpenVideoSource(format, url string, options map[string]string, codec string) (*Session, error) {
	if codec != "h264" && codec != "hevc" {
		return OpenSource(format, url, options, false)
	}
	requested := make(map[string]string, len(options)+1)
	for k, v := range options {
		requested[k] = v
	}
	if format == "v4l2" {
		requested["input_format"] = codec
	}
	source, err := openSource(format, url, requested, false, codec)
	if err != nil && strings.Contains(err.Error(), "open capture:") && (format == "v4l2" || format == "dshow") {
		// The device may offer only raw/MJPEG at this configured resolution/rate.
		return OpenSource(format, url, options, false)
	}
	return source, err
}
func openSource(format, url string, options map[string]string, audio bool, codec string) (*Session, error) {
	if err := Available(); err != nil {
		return nil, err
	}
	var pairs []string
	for key, value := range options {
		if strings.ContainsAny(key, "=\n\x00") || strings.ContainsAny(value, "\n\x00") {
			return nil, fmt.Errorf("invalid device option")
		}
		pairs = append(pairs, key+"="+value)
	}
	f, u, o := C.CString(format), C.CString(url), C.CString(strings.Join(pairs, "\n"))
	defer C.free(unsafe.Pointer(f))
	defer C.free(unsafe.Pointer(u))
	defer C.free(unsafe.Pointer(o))
	a := 0
	if audio {
		a = 1
	}
	var msg [512]C.char
	var thread *mediaThread
	if runtime.GOOS == "windows" {
		thread = newMediaThread()
	}
	requestedCodec := C.CString(codec)
	defer C.free(unsafe.Pointer(requestedCodec))
	var p *C.bm_session
	thread.call(func() { p = C.bm_source(f, u, o, C.int(a), requestedCodec, &msg[0], 512) })
	s, err := resultSession(p, &msg[0])
	if err != nil {
		thread.close()
		return nil, err
	}
	s.thread = thread
	var num, den C.int
	C.bm_source_rate(p, &num, &den)
	s.sourceRate = framerate.New(int(num), int(den))
	switch C.bm_source_codec(p) {
	case 1:
		s.sourceCodec = "h264"
	case 2:
		s.sourceCodec = "hevc"
	}
	return s, nil
}
func DeviceLog(format string) []string {
	if Available() != nil {
		return nil
	}
	f := C.CString(format)
	defer C.free(unsafe.Pointer(f))
	p := C.bm_devices(f)
	if p == nil {
		return nil
	}
	defer C.free(unsafe.Pointer(p))
	return strings.Split(C.GoString(p), "\n")
}
func (s *Session) Close() {
	s.life.Lock()
	defer s.life.Unlock()
	if s.ptr == nil {
		return
	}
	C.bm_interrupt(s.ptr)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.thread.call(func() { C.bm_close(s.ptr) })
	s.thread.close()
	s.ptr = nil
}
func (s *Session) lock() bool {
	s.life.Lock()
	s.mu.Lock()
	s.life.Unlock()
	if s.ptr == nil {
		s.mu.Unlock()
		return false
	}
	return true
}
func (s *Session) Encode(data []byte, w, h int, pixfmt string, idr bool) ([]byte, error) {
	b, err := s.EncodeOwned(data, w, h, pixfmt, idr)
	if err != nil || b == nil {
		return nil, err
	}
	defer b.Release()
	return append([]byte(nil), b.Bytes()...), nil
}

// EncodeOwned transfers immutable native output to the caller. Its lifetime is
// independent of this session; Release after the last reader finishes.
func (s *Session) EncodeOwned(data []byte, w, h int, pixfmt string, idr bool) (*framebuf.Buffer, error) {
	if !s.lock() {
		return nil, io.ErrClosedPipe
	}
	defer s.mu.Unlock()
	if len(data) == 0 {
		return nil, fmt.Errorf("empty input frame")
	}
	p := C.CString(pixfmt)
	defer C.free(unsafe.Pointer(p))
	var out *C.uint8_t
	var size C.int
	var msg [512]C.char
	key := 0
	if idr {
		key = 1
	}
	result := C.bm_encode(s.ptr, (*C.uint8_t)(unsafe.Pointer(&data[0])), C.size_t(len(data)), C.int(w), C.int(h), p, C.int(key), &out, &size, &msg[0], 512)
	runtime.KeepAlive(data)
	return outputOwned(result, out, size, &msg[0])
}
func output(result C.int, out *C.uint8_t, size C.int, msg *C.char) ([]byte, error) {
	b, err := outputOwned(result, out, size, msg)
	if err != nil || b == nil {
		return nil, err
	}
	defer b.Release()
	return append([]byte(nil), b.Bytes()...), nil
}
func outputOwned(result C.int, out *C.uint8_t, size C.int, msg *C.char) (*framebuf.Buffer, error) {
	defer func() {
		if out != nil {
			C.free(unsafe.Pointer(out))
		}
	}()
	if result == 1 {
		return nil, io.EOF
	}
	if result < 0 {
		return nil, fmt.Errorf("libav: %s", C.GoString(msg))
	}
	if size == 0 {
		return nil, nil
	}
	if size < 0 || size > 128*1024*1024 || out == nil {
		return nil, fmt.Errorf("invalid media output size")
	}
	owned := out
	data := unsafe.Slice((*byte)(unsafe.Pointer(out)), int(size))
	out = nil
	return framebuf.New(data, func() { C.free(unsafe.Pointer(owned)) }), nil
}
func (s *Session) Decode(data []byte) (*Frame, error) {
	if !s.lock() {
		return nil, io.ErrClosedPipe
	}
	defer s.mu.Unlock()
	if len(data) == 0 {
		return nil, nil
	}
	var out *C.uint8_t
	var size, w, h C.int
	var msg [512]C.char
	result := C.bm_decode(s.ptr, (*C.uint8_t)(unsafe.Pointer(&data[0])), C.size_t(len(data)), &out, &size, &w, &h, &msg[0], 512)
	bytes, err := output(result, out, size, &msg[0])
	if err != nil || len(bytes) == 0 {
		return nil, err
	}
	return &Frame{Data: bytes, Width: int(w), Height: int(h)}, nil
}
func (s *Session) Read() (*Frame, error) {
	f, err := s.ReadOwned()
	if err != nil || f == nil {
		return nil, err
	}
	data := append([]byte(nil), f.Data...)
	f.Release()
	f.Data = data
	return f, nil
}

// ReadOwned avoids copying native output into the Go heap. Release the frame
// when consumed, or transfer its Buffer reference to another owner.
func (s *Session) ReadOwned() (*Frame, error) {
	if !s.lock() {
		return nil, io.ErrClosedPipe
	}
	defer s.mu.Unlock()
	var out *C.uint8_t
	var size, w, h C.int
	var msg [512]C.char
	var result C.int
	s.thread.call(func() { result = C.bm_read(s.ptr, &out, &size, &w, &h, &msg[0], 512) })
	buffer, err := outputOwned(result, out, size, &msg[0])
	if err != nil || buffer == nil {
		return nil, err
	}
	return &Frame{Buffer: buffer, Data: buffer.Bytes(), Width: int(w), Height: int(h), Codec: s.sourceCodec}, nil
}

func (s *Session) FrameRate() framerate.Rate { return s.sourceRate }
