// Package scrcpy connects AnyKVM to an Android device using the scrcpy
// server (https://github.com/Genymobile/scrcpy). The server jar is pushed to
// the device over adb and started via app_process; it streams the screen as
// H.264/H.265 over a forwarded socket, optionally forwards system audio, and
// accepts input-injection control messages. Requires adb and scrcpy (>= 3.0) for the
// scrcpy-server jar.
package scrcpy

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"anykvm/internal/capture"
)

// Options configure an Android scrcpy session.
type Options struct {
	Serial                string // adb device serial ("" = the only connected device)
	BitRate               int    // video bps; positive GameStream ceiling; 0 = no ceiling / Web/VNC server default
	ServerJar             string // jar path relative to executable directory ("" = find beside executable)
	Version               string // scrcpy server version ("" = auto-detect)
	StayAwake             bool   // keep the device awake while connected
	KeepActive            bool   // signal user activity without changing global settings (scrcpy >= 4.0)
	Audio                 bool   // forward device system audio as 48kHz stereo PCM
	Crop                  string // width:height:x:y in the device's natural orientation ("" = full display)
	GameStreamOrientation string // capture orientation override, only for GameStream
}

const (
	remoteJarPath = "/data/local/tmp/anykvm-scrcpy-server.jar"
	codecIDH264   = 0x68323634 // "h264" codec id in the scrcpy codec metadata
	codecIDH265   = 0x68323635
	codecIDRaw    = 0x00726177

	// Frame packet flags (high bits of the PTS field).
	packetFlagConfig   = uint64(1) << 63
	packetFlagKeyFrame = uint64(1) << 62
	packetFlagSession  = uint64(1) << 63 // scrcpy >= 4.0
	packetFlagConfigV4 = uint64(1) << 62

	maxPacketSize = 16 << 20
)

// Session manages one scrcpy server instance on an Android device: adb setup,
// the video stream socket, and the control (input injection) socket. It
// implements the VNC/RDP/WS HIDSender interface for keyboard/mouse input.
type Session struct {
	opts Options

	mu                 sync.Mutex // guards lifecycle state below
	running            bool
	adbPath            string
	scid               string
	port               int
	server             *exec.Cmd
	video              net.Conn
	audio              net.Conn
	audioFrames        chan *capture.AudioFrame
	audioEpoch         uint64
	videoCodec         string // Android encoder name: h264 or h265
	displayWidth       int    // explicit video.mode dimensions; zero mirrors the device
	displayHeight      int
	control            net.Conn
	vr                 *bufio.Reader
	fps                int
	captureOrientation string // selected by Capture for the current consumer
	captureBitRate     int    // selected encoder bitrate in bps (0 = server default)
	sessionPackets     bool   // scrcpy >= 4.0 sends dimensions as session packets
	scroll16           bool   // scrcpy >= 3.3.1 uses a [-16, 16] scroll range

	readMu sync.Mutex // serializes video packet reads
	ctlMu  sync.Mutex // serializes control socket writes

	sizeMu sync.Mutex
	width  int
	height int

	inMu    sync.Mutex // guards input state below
	pressed map[uint8]bool
	buttons uint32
	lastX   int // last pointer position, normalized 0..32767
	lastY   int
}

// NewSession creates an unstarted scrcpy session.
func NewSession(opts Options) *Session {
	return &Session{opts: opts, videoCodec: "h264", captureBitRate: opts.BitRate, pressed: make(map[uint8]bool)}
}

// IsAndroidDevice reports whether deviceID selects an Android device via
// scrcpy: "adb" or "adb:<serial>".
func IsAndroidDevice(deviceID string) bool {
	return deviceID == "adb" || strings.HasPrefix(deviceID, "adb:")
}

// DeviceSerial extracts the adb serial from an android device id ("" = default).
func DeviceSerial(deviceID string) string {
	if i := strings.IndexByte(deviceID, ':'); i >= 0 {
		return deviceID[i+1:]
	}
	return ""
}

// Start pushes the scrcpy server to the device, launches it, and connects the
// video, optional audio, and control sockets. maxSize limits the larger video dimension
// (0 = device native); fps limits the frame rate (0 = unlimited). Idempotent
// while running.
func (s *Session) Start(maxSize, fps int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return nil
	}
	if err := ValidateCaptureOrientation(s.captureOrientation); err != nil {
		return err
	}
	if err := ValidateCrop(s.opts.Crop); err != nil {
		return fmt.Errorf("scrcpy.crop: %w", err)
	}

	adb, err := findADB()
	if err != nil {
		return err
	}
	s.adbPath = adb

	jar, version, err := resolveServer(s.opts)
	if err != nil {
		return err
	}
	if versionLess(parseVersionParts(version), []int{3, 0}) {
		return fmt.Errorf("scrcpy: scrcpy-server >= 3.0 is required (got %s)", version)
	}
	s.sessionPackets = !versionLess(parseVersionParts(version), []int{4, 0})
	s.scroll16 = !versionLess(parseVersionParts(version), []int{3, 3, 1})
	if s.opts.KeepActive && !s.sessionPackets {
		return fmt.Errorf("scrcpy.keep_active requires scrcpy-server >= 4.0 (got %s)", version)
	}

	if out, err := s.adbCmd("push", jar, remoteJarPath).CombinedOutput(); err != nil {
		return fmt.Errorf("scrcpy: adb push: %v: %s", err, strings.TrimSpace(string(out)))
	}

	scid := randomSCID()
	socketName := "localabstract:scrcpy_" + scid

	out, err := s.adbCmd("forward", "tcp:0", socketName).Output()
	if err != nil {
		return fmt.Errorf("scrcpy: adb forward: %w", err)
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || port <= 0 {
		return fmt.Errorf("scrcpy: adb forward: unexpected output %q", strings.TrimSpace(string(out)))
	}

	args := s.serverArgs(version, scid, maxSize, fps)
	server := s.adbCmd(args...)
	logCmdOutput(server, "scrcpy-server")
	log.Printf("scrcpy: launching server v%s (scid=%s, port=%d, crop=%q, capture_orientation=%q, video_bit_rate=%d bps)", version, scid, port, s.opts.Crop, s.captureOrientation, s.captureBitRate)
	if err := server.Start(); err != nil {
		s.removeForward(port)
		return fmt.Errorf("scrcpy: start server: %w", err)
	}

	fail := func(e error) error {
		if server.Process != nil {
			server.Process.Kill()
		}
		s.removeForward(port)
		return e
	}

	video, err := connectVideo(port, 10*time.Second)
	if err != nil {
		return fail(err)
	}

	// All sockets must connect in order (video, optional audio, control) before
	// reading metadata, since the server waits for them before streaming.
	var audio net.Conn
	if s.opts.Audio {
		audio, err = net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 3*time.Second)
		if err != nil {
			video.Close()
			return fail(fmt.Errorf("scrcpy: connect audio socket: %w", err))
		}
	}
	closeSockets := func() {
		video.Close()
		if audio != nil {
			audio.Close()
		}
	}
	control, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 3*time.Second)
	if err != nil {
		closeSockets()
		return fail(fmt.Errorf("scrcpy: connect control socket: %w", err))
	}

	vr := bufio.NewReaderSize(video, 128*1024)
	video.SetReadDeadline(time.Now().Add(10 * time.Second))
	codecID, w, h, err := readVideoHeader(vr, s.sessionPackets)
	video.SetReadDeadline(time.Time{})
	if err != nil {
		closeSockets()
		control.Close()
		return fail(fmt.Errorf("scrcpy: read codec metadata: %w", err))
	}
	wantCodec := uint32(codecIDH264)
	if s.videoCodec == "h265" {
		wantCodec = codecIDH265
	}
	if codecID != wantCodec {
		closeSockets()
		control.Close()
		return fail(fmt.Errorf("scrcpy: unexpected video codec id 0x%08x (want %s)", codecID, s.videoCodec))
	}

	s.scid = scid
	s.port = port
	s.server = server
	s.video = video
	s.audio = audio
	s.audioFrames = nil
	if audio != nil {
		s.audioEpoch++
		s.audioFrames = make(chan *capture.AudioFrame, 5)
		go receiveAudio(audio, s.audioFrames, s.audioEpoch, s.sessionPackets)
	}
	s.control = control
	s.vr = vr
	s.fps = fps
	s.setSize(w, h)
	s.running = true
	log.Printf("scrcpy: connected to device (%dx%d)", w, h)
	return nil
}

// Stop tears down the session: sockets, server process, and adb forward.
func (s *Session) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return
	}
	s.running = false
	if s.audio != nil {
		s.audio.Close()
		s.audio = nil
	}
	s.audioFrames = nil
	if s.control != nil {
		s.control.Close()
		s.control = nil
	}
	if s.video != nil {
		s.video.Close()
		s.video = nil
	}
	s.vr = nil
	if s.server != nil && s.server.Process != nil {
		s.server.Process.Kill()
	}
	s.server = nil
	s.removeForward(s.port)
	s.port = 0

	s.inMu.Lock()
	s.pressed = make(map[uint8]bool)
	s.buttons = 0
	s.inMu.Unlock()
	log.Printf("scrcpy: session stopped")
}

// Running reports whether the session is currently connected.
func (s *Session) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Size returns the current video dimensions.
func (s *Session) Size() (w, h int) {
	s.sizeMu.Lock()
	defer s.sizeMu.Unlock()
	return s.width, s.height
}

func (s *Session) setSize(w, h int) {
	s.sizeMu.Lock()
	if w != s.width || h != s.height {
		if s.width != 0 || s.height != 0 {
			log.Printf("scrcpy: video size changed %dx%d -> %dx%d", s.width, s.height, w, h)
		}
		s.width, s.height = w, h
	}
	s.sizeMu.Unlock()
}

// FPS returns the frame rate requested at Start (0 = unlimited/unknown).
func (s *Session) FPS() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fps
}

// ReadPacket reads the next video packet: a 12-byte frame header (PTS+flags
// u64, length u32) followed by H.264/H.265 Annex-B data. config reports whether the
// packet carries codec parameter sets instead of a coded frame.
// On scrcpy >= 4.0, intervening session packets update dimensions and are skipped.
func (s *Session) ReadPacket() (data []byte, config bool, err error) {
	s.mu.Lock()
	vr := s.vr
	sessionPackets := s.sessionPackets
	s.mu.Unlock()
	if vr == nil {
		return nil, false, fmt.Errorf("scrcpy: session not running")
	}

	s.readMu.Lock()
	defer s.readMu.Unlock()

	for {
		var hdr [12]byte
		if _, err := io.ReadFull(vr, hdr[:]); err != nil {
			return nil, false, err
		}
		ptsFlags := binary.BigEndian.Uint64(hdr[0:8])
		configFlag := packetFlagConfig
		if sessionPackets {
			if ptsFlags&packetFlagSession != 0 {
				w, h, err := readSessionSize(hdr[:])
				if err != nil {
					return nil, false, err
				}
				s.setSize(w, h)
				continue
			}
			configFlag = packetFlagConfigV4
		}
		size := binary.BigEndian.Uint32(hdr[8:12])
		if size == 0 || size > maxPacketSize {
			return nil, false, fmt.Errorf("scrcpy: invalid packet size %d", size)
		}
		buf := make([]byte, size)
		if _, err := io.ReadFull(vr, buf); err != nil {
			return nil, false, err
		}
		return buf, ptsFlags&configFlag != 0, nil
	}
}

// adbCmd builds an adb command targeting the configured device serial.
func (s *Session) adbCmd(args ...string) *exec.Cmd {
	if s.opts.Serial != "" {
		args = append([]string{"-s", s.opts.Serial}, args...)
	}
	return exec.Command(s.adbPath, args...)
}

func (s *Session) removeForward(port int) {
	if port <= 0 {
		return
	}
	s.adbCmd("forward", "--remove", fmt.Sprintf("tcp:%d", port)).Run()
}

// connectVideo dials the forwarded port until the server's dummy byte arrives.
// With a forward tunnel the local TCP connect succeeds even before the server
// is listening on the device, so only a successful 1-byte read confirms the
// video socket is live.
func connectVideo(port int, timeout time.Duration) (net.Conn, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.SetReadDeadline(time.Now().Add(time.Second))
			var b [1]byte
			if _, err = io.ReadFull(conn, b[:]); err == nil {
				conn.SetReadDeadline(time.Time{})
				return conn, nil
			}
			conn.Close()
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("scrcpy: server did not come up within %s (last error: %v)", timeout, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// readCodecMeta reads the codec metadata sent at the start of the video
// socket: codec id (u32), width (u32), height (u32).
func readCodecMeta(r io.Reader) (codecID uint32, w, h int, err error) {
	var buf [12]byte
	if _, err = io.ReadFull(r, buf[:]); err != nil {
		return 0, 0, 0, err
	}
	codecID = binary.BigEndian.Uint32(buf[0:4])
	w = int(binary.BigEndian.Uint32(buf[4:8]))
	h = int(binary.BigEndian.Uint32(buf[8:12]))
	return codecID, w, h, nil
}

// Since scrcpy 4.0, only the codec id precedes the first session packet.
// Session packets carry dimensions and recur whenever capture restarts.
func readVideoHeader(r io.Reader, sessionPackets bool) (codecID uint32, w, h int, err error) {
	if !sessionPackets {
		return readCodecMeta(r)
	}
	var buf [16]byte
	if _, err = io.ReadFull(r, buf[:]); err != nil {
		return 0, 0, 0, err
	}
	codecID = binary.BigEndian.Uint32(buf[:4])
	w, h, err = readSessionSize(buf[4:])
	return
}

func readSessionSize(header []byte) (w, h int, err error) {
	if binary.BigEndian.Uint32(header[:4])&0x80000000 == 0 {
		return 0, 0, fmt.Errorf("scrcpy: expected video session packet")
	}
	w = int(binary.BigEndian.Uint32(header[4:8]))
	h = int(binary.BigEndian.Uint32(header[8:12]))
	if w <= 0 || h <= 0 || w > 65535 || h > 65535 {
		return 0, 0, fmt.Errorf("scrcpy: invalid video session size %dx%d", w, h)
	}
	return w, h, nil
}

// randomSCID generates the 31-bit scrcpy session id as 8 lowercase hex digits.
func randomSCID() string {
	return fmt.Sprintf("%08x", rand.Int31())
}

// logCmdOutput forwards the command's stdout/stderr lines to the log with a
// tag, and reaps the process when the streams end. Must be called before Start.
func logCmdOutput(cmd *exec.Cmd, tag string) {
	stdout, err1 := cmd.StdoutPipe()
	stderr, err2 := cmd.StderrPipe()
	if err1 != nil || err2 != nil {
		return
	}
	var wg sync.WaitGroup
	scan := func(r io.Reader) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line != "" {
				log.Printf("%s: %s", tag, line)
			}
		}
	}
	wg.Add(2)
	go scan(stdout)
	go scan(stderr)
	go func() {
		wg.Wait()
		cmd.Wait()
	}()
}
