package capture

import (
	"anykvm/internal/avmedia"
	"anykvm/internal/framerate"
	"anykvm/internal/videoenc"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// ResolveFFmpeg reports whether the FFmpeg shared libraries are available.
// The returned name identifies the library, not an executable.
func ResolveFFmpeg() (string, bool) { return resolveFFmpeg() }
func resolveFFmpeg() (string, bool) {
	if avmedia.Available() != nil {
		return "", false
	}
	return "libavcodec", true
}
func ffmpegAvailable() bool { return avmedia.Available() == nil }

// ringBuffer keeps the last max bytes written for diagnostics.
type ringBuffer struct {
	mu   sync.Mutex
	data []byte
	max  int
}

func newRingBuffer(max int) *ringBuffer {
	return &ringBuffer{max: max}
}

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.data = append(r.data, p...)
	if len(r.data) > r.max {
		r.data = r.data[len(r.data)-r.max:]
	}
	return len(p), nil
}

func (r *ringBuffer) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.TrimSpace(string(r.data))
}

// ffmpegDevice captures video through in-process libavdevice/libavcodec.
// Works on Windows (DirectShow), macOS (AVFoundation), Linux (V4L2).
type ffmpegDevice struct {
	device              string
	jpegMedia           *avmedia.Session
	jpegW, jpegH        int
	media               *avmedia.Session
	encoder             videoenc.Encoder
	encodeOptions       videoenc.Options
	encodedFrames       map[uint64]*Frame
	nextEncodedID       uint64
	pendingFrame        *Frame
	forceIDR            atomic.Bool
	passthrough         atomic.Bool
	scalableInput       bool
	variableInputRate   bool // USB mirroring's nominal device rate may differ from sample delivery.
	inputFPS            framerate.Rate
	requestedRate       framerate.Rate
	videoBitrate        int
	sourceFrame         uint64
	skippedEncodeInputs uint64 // unencoded inputs explicitly rejected by pacing/decimation
	targetW, targetH    int
	quality             int
	encodePacer         framePacer
	stderr              *ringBuffer // captures ffmpeg's stderr for diagnostics
	mu                  sync.Mutex
	width               int
	height              int
	reqFPS              int // frame rate requested at Open (>0 = explicit; 0 = device default)

	streamOptions    *VideoStreamOptions
	sourceW, sourceH int
	videoCodec       string      // "" / "mjpeg" = MJPEG output; "h264" = Annex-B H.264; "rawvideo" = BGRA pixels
	videoChroma      string      // H.264 chroma: "" / "420" = 4:2:0; "422" = 4:2:2
	closed           atomic.Bool // set by Close to distinguish shutdown from capture failure

	// argsFunc, if set, overrides the default capture-card argument builder
	// (used by screen capture to supply platform-specific screen-grab args).
	argsFunc func(width, height, fps int) []string

	qscale string // ffmpeg mjpeg -q:v value ("" = default)

	decodeMu    sync.Mutex
	decodeImage bool // whether to decode MJPEG frames into image.Image
}

// NewFFmpegDevice creates a capture device backed by ffmpeg.
// device: DirectShow name (Windows), AVFoundation index (macOS), or /dev/video path (Linux).
// Use empty string for default device.
func NewFFmpegDevice(device string) Device {
	if device == "" {
		device = "0"
	}
	return &ffmpegDevice{device: device, decodeImage: true}
}

// SetDecodeImage toggles JPEG decoding of captured MJPEG frames.
func (d *ffmpegDevice) SetDecodeImage(v bool) {
	d.decodeMu.Lock()
	d.decodeImage = v
	d.decodeMu.Unlock()
}

// SetJPEGQuality stores the MJPEG quality (1-100) as an FFmpeg qscale value.
func (d *ffmpegDevice) SetJPEGQuality(q int) {
	d.qscale = jpegQualityToQV(q)
}

// SetVideoCodec selects the capture output codec: "" or "mjpeg" for MJPEG
// (default), "h264" for Annex-B H.264, "rawvideo" for uncompressed BGRA.
// Takes effect on the next Open.
func (d *ffmpegDevice) SetVideoCodec(codec string) {
	d.videoCodec = codec
}

// SetVideoChroma selects the H.264 chroma subsampling: "" / "420" = 4:2:0,
// "422" = 4:2:2. Takes effect on the next Open.
func (d *ffmpegDevice) SetVideoChroma(chroma string) {
	d.videoChroma = chroma
}

func (d *ffmpegDevice) h264Mode() bool     { return d.videoCodec == "h264" || d.videoCodec == "hevc" }
func (d *ffmpegDevice) rawvideoMode() bool { return d.videoCodec == "rawvideo" }
func (d *ffmpegDevice) hevcMode() bool     { return d.videoCodec == "hevc" }
func (d *ffmpegDevice) uyvyMode() bool     { return d.videoCodec == "uyvy" }

// CaptureFPS reports the negotiated capture frame rate. If an explicit rate was
// requested at Open it is returned directly; otherwise the actual rate is parsed
// from ffmpeg's stream info (captured on stderr). Returns 0 when unknown.
func (d *ffmpegDevice) CaptureFPS() int {
	if d.reqFPS > 0 {
		return d.reqFPS
	}
	if d.stderr != nil {
		return parseFPSFromLog(d.stderr.String())
	}
	return 0
}

var (
	fpsLogRe  = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)\s+fps(?:,|\s|$)`)
	tbrLogRe  = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)\s+tbr(?:,|\s|$)`)
	sizeLogRe = regexp.MustCompile(`(\d{2,5})x(\d{2,5})`)
)

// parseFPSFromLog reads the first frame rate, preferring fps over tbr.
// FFmpeg logs the input stream first. No match returns zero.
func parseFPSFromLog(log string) int {
	round := func(m []string) int {
		if m == nil {
			return 0
		}
		f, err := strconv.ParseFloat(m[1], 64)
		if err != nil || f <= 0 {
			return 0
		}
		return int(f + 0.5)
	}
	if v := round(fpsLogRe.FindStringSubmatch(log)); v > 0 {
		return v
	}
	return round(tbrLogRe.FindStringSubmatch(log))
}

// parseSizeFromLog returns the last valid WxH pair, normally the output size.
// Digit bounds exclude addresses such as "0x7f...". No match returns 0, 0.
func parseSizeFromLog(log string) (w, h int) {
	matches := sizeLogRe.FindAllStringSubmatch(log, -1)
	for i := len(matches) - 1; i >= 0; i-- {
		pw, _ := strconv.Atoi(matches[i][1])
		ph, _ := strconv.Atoi(matches[i][2])
		if pw >= 16 && ph >= 16 && pw <= 16384 && ph <= 16384 {
			return pw, ph
		}
	}
	return 0, 0
}

// Only accept a complete output video stream line. Input dimensions may be
// logged much earlier and can differ after aspect-preserving scaling.
func parseOutputSizeFromLog(log string) (int, int) {
	output := strings.LastIndex(log, "Output #0,")
	if output < 0 {
		return 0, 0
	}
	lines := strings.Split(log[output:], "\n")
	for _, line := range lines[:len(lines)-1] {
		if strings.Contains(line, "Stream #0:") && strings.Contains(line, "Video:") {
			return parseSizeFromLog(line)
		}
	}
	return 0, 0
}

func (d *ffmpegDevice) shouldDecode() bool {
	d.decodeMu.Lock()
	defer d.decodeMu.Unlock()
	return d.decodeImage
}

func (d *ffmpegDevice) Open() error {
	return d.OpenWithFormat(0, 0, 30)
}

func (d *ffmpegDevice) OpenWithFormat(width, height, fps int) error {
	if !avmedia.Enabled {
		return avmedia.Available()
	}
	d.reqFPS = fps
	var args []string
	if d.argsFunc != nil {
		args = d.argsFunc(width, height, fps)
	} else {
		args = buildFFmpegArgs(d.device, width, height, fps)
	}
	return d.openArgs("", args, width, height)
}

func (d *ffmpegDevice) openArgs(_ string, args []string, width, height int) error {
	format, url, options, err := mediaInputArgs(args)
	if err != nil {
		return err
	}
	codec := d.videoCodec
	if d.streamOptions != nil {
		codec = d.streamOptions.Codec
		if codec == "" {
			codec = "h264"
		}
	}
	source, err := avmedia.OpenVideoSource(format, url, options, codec)
	if err != nil {
		return err
	}
	d.media = source
	d.inputFPS = source.FrameRate()
	d.targetW = width
	d.targetH = height
	d.closed.Store(false)
	frame, err := source.ReadOwned()
	if err != nil {
		source.Close()
		return err
	}
	if frame == nil {
		source.Close()
		return fmt.Errorf("capture produced no initial frame")
	}
	d.pendingFrame = mediaCaptureFrame(frame)
	d.passthrough.Store(frame.Codec != "")
	if frame.Codec != "" {
		log.Printf("capture: %s passthrough %dx%d; preserving source rate and bitrate", frame.Codec, frame.Width, frame.Height)
	} else if codec == "h264" || codec == "hevc" || codec == "av1" {
		log.Printf("capture: no matching compressed source; encoding %s from pixels", codec)
	}
	d.sourceW = frame.Width
	d.sourceH = frame.Height
	d.width, d.height = frame.Width, frame.Height
	if !d.PreserveCaptureMode() {
		d.width, d.height = fitCaptureSize(frame.Width, frame.Height, width, height)
	}
	return nil
}

// dropArgPair returns args with the first occurrence of flag and its value
// removed, and whether a removal happened.
func dropArgPair(args []string, flag string) ([]string, bool) {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			out := append(append([]string{}, args[:i]...), args[i+2:]...)
			return out, true
		}
	}
	return args, false
}

// readFirstFrame reads the initial frame with a timeout so startup cannot hang.

// lastLines returns up to n trailing non-empty lines of s.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// overrideQV replaces the value following "-q:v" in args with qv.
func overrideQV(args []string, qv string) []string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-q:v" {
			args[i+1] = qv
			return args
		}
	}
	return args
}

// framerateArgs returns ["-framerate", "N"] when fps > 0, else nil (let the
// device use its default frame rate).
func framerateArgs(fps int) []string {
	if fps > 0 {
		return []string{"-framerate", strconv.Itoa(fps)}
	}
	return nil
}

// outputRateArgs returns ["-r", "N"] when fps > 0, else nil.
func outputRateArgs(fps int) []string {
	if fps > 0 {
		return []string{"-r", strconv.Itoa(fps)}
	}
	return nil
}

// videoSizeArgs returns ["-video_size", "WxH"] when both > 0, else nil.
func videoSizeArgs(width, height int) []string {
	if width > 0 && height > 0 {
		return []string{"-video_size", fmt.Sprintf("%dx%d", width, height)}
	}
	return nil
}

// scaleFilterArgs returns ["-vf", "scale=..."] downscaling to width x height
// (preserving aspect ratio, even dimensions), else nil.
//
// Uses the fast_bilinear scaler: for large downscales (e.g. Retina -> 1080p)
// it is significantly cheaper on CPU than the default bicubic, with negligible
// quality difference for screen content.
func scaleFilterArgs(width, height int) []string {
	if width > 0 && height > 0 {
		vf := fmt.Sprintf("scale=w=%d:h=%d:force_original_aspect_ratio=decrease:flags=fast_bilinear,scale=trunc(iw/2)*2:trunc(ih/2)*2",
			width, height)
		return []string{"-vf", vf}
	}
	return nil
}

func (d *ffmpegDevice) Capture() (*Frame, error) {
	return d.readFrame()
}

// readRawvideoFrame reads a single raw BGRA frame from the pipe. Each frame is
// exactly width*height*4 bytes with no framing or delimiters.

// readUYVYFrame reads a single raw UYVY 4:2:2 frame from the pipe. Each frame is
// exactly width*height*2 bytes (2 bytes/pixel).

// fillH264 reads from the H.264 pipe and feeds raw bytes to the Annex-B
// access-unit parser. Returns io.EOF when the stream ends.
// No idle-flush, no deadline — the push-based parser naturally emits complete
// AUs when the next frame's start code arrives.

// readH264Frame returns the next complete H.264 access unit from the ffmpeg
// pipe, blocking until one is available. Caller holds d.mu.

// primeH264 reads from the ffmpeg pipe until the parser has at least one
// complete access unit buffered, bounded by timeout.

func (d *ffmpegDevice) Close() error {
	d.closed.Store(true)
	if d.media != nil {
		d.media.Close()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pendingFrame.Release()
	d.pendingFrame = nil
	if d.jpegMedia != nil {
		d.jpegMedia.Close()
		d.jpegMedia = nil
	}
	if d.encoder != nil {
		d.encoder.Close()
		d.encoder = nil
	}
	d.encodedFrames = nil
	return nil
}

func (d *ffmpegDevice) readFrame() (*Frame, error)         { return d.readMediaFrame() }
func (d *ffmpegDevice) readRawvideoFrame() (*Frame, error) { return d.readMediaFrame() }

func (d *ffmpegDevice) CaptureFrameRate() framerate.Rate {
	if d.inputFPS.Valid() {
		return d.inputFPS
	}
	return framerate.New(d.reqFPS, 1)
}
