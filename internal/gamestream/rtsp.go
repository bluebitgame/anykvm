package gamestream

import (
	"anykvm/internal/netstats"
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

const maxRTSPMessage = 32 * 1024

type rtspRequest struct {
	method, target, cseq string
	headers              textproto.MIMEHeader
	body                 []byte
}

func parseRTSP(r io.Reader) (rtspRequest, error) {
	reader := textproto.NewReader(bufio.NewReader(io.LimitReader(r, maxRTSPMessage)))
	line, err := reader.ReadLine()
	if err != nil {
		return rtspRequest{}, err
	}
	parts := strings.Fields(line)
	if len(parts) != 3 || parts[2] != "RTSP/1.0" {
		return rtspRequest{}, errors.New("invalid RTSP request line")
	}
	headers, err := reader.ReadMIMEHeader()
	if err != nil {
		return rtspRequest{}, err
	}
	cseq := headers.Get("CSeq")
	if _, err := strconv.ParseUint(cseq, 10, 32); err != nil {
		return rtspRequest{}, errors.New("invalid CSeq")
	}
	length := 0
	if v := headers.Get("Content-Length"); v != "" {
		length, err = strconv.Atoi(v)
	}
	if err != nil || length < 0 || length > maxRTSPMessage/2 {
		return rtspRequest{}, errors.New("invalid RTSP body length")
	}
	if len(headers.Values("Content-Length")) > 1 || len(headers.Values("CSeq")) != 1 {
		return rtspRequest{}, errors.New("duplicate RTSP framing header")
	}
	body := make([]byte, length)
	if _, err = io.ReadFull(reader.R, body); err != nil {
		return rtspRequest{}, err
	}
	return rtspRequest{parts[0], parts[1], cseq, headers, body}, nil
}

func rtspResponse(code int, cseq, body string, extra map[string]string) []byte {
	status := map[int]string{200: "OK", 400: "Bad Request", 454: "Session Not Found", 455: "Method Not Valid In This State", 461: "Unsupported Transport", 500: "Internal Server Error", 501: "Not Implemented"}[code]
	var b strings.Builder
	fmt.Fprintf(&b, "RTSP/1.0 %d %s\r\nCSeq: %s\r\nContent-Length: %d\r\n", code, status, cseq, len(body))
	for k, v := range extra {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	b.WriteString("\r\n")
	b.WriteString(body)
	return []byte(b.String())
}

func rtspNonce(seq uint32, side byte) []byte {
	iv := make([]byte, 12)
	binary.LittleEndian.PutUint32(iv, seq)
	iv[10] = side
	iv[11] = 'R'
	return iv
}

func (x *streamSession) readRTSP(conn net.Conn) (rtspRequest, error) {
	if !x.rtspEncrypted {
		return parseRTSP(conn)
	}
	header := make([]byte, 24)
	if _, err := io.ReadFull(conn, header); err != nil {
		return rtspRequest{}, err
	}
	typeLength := binary.BigEndian.Uint32(header)
	length := typeLength & 0x7fffffff
	if typeLength&0x80000000 == 0 || length == 0 || length > maxRTSPMessage {
		return rtspRequest{}, errors.New("invalid encrypted RTSP envelope")
	}
	data := make([]byte, int(length)+16)
	if _, err := io.ReadFull(conn, data[:length]); err != nil {
		return rtspRequest{}, err
	}
	copy(data[length:], header[8:24])
	seq := binary.BigEndian.Uint32(header[4:8])
	plain, err := x.gcm.Open(nil, rtspNonce(seq, 'C'), data, nil)
	if err != nil {
		return rtspRequest{}, err
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if seq <= x.rtspIn {
		return rtspRequest{}, errors.New("replayed RTSP request")
	}
	x.rtspIn = seq
	return parseRTSP(bytes.NewReader(plain))
}

func (x *streamSession) writeRTSP(conn net.Conn, response []byte) error {
	// Capture startup may exhaust the request's read budget. Start a fresh
	// write budget only after negotiation has produced a response.
	if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	if x.rtspEncrypted {
		x.mu.Lock()
		x.rtspOut++
		seq := x.rtspOut
		x.mu.Unlock()
		if seq == 0 {
			return errors.New("RTSP nonce exhausted")
		}
		sealed := x.gcm.Seal(nil, rtspNonce(seq, 'H'), response, nil)
		out := make([]byte, 24+len(response))
		binary.BigEndian.PutUint32(out, uint32(len(response))|0x80000000)
		binary.BigEndian.PutUint32(out[4:], seq)
		copy(out[8:24], sealed[len(response):])
		copy(out[24:], sealed[:len(response)])
		response = out
	}
	_, err := io.Copy(conn, bytes.NewReader(response))
	return err
}

func (s *Server) rtspLoop() {
	limit := make(chan struct{}, 8)
	for {
		conn, err := s.rtspListener.Accept()
		if err != nil {
			return
		}
		conn = netstats.WrapConn(conn)
		select {
		case limit <- struct{}{}:
		default:
			conn.Close()
			continue
		}
		s.rtspHandlers.Add(1)
		go func() {
			defer s.rtspHandlers.Done()
			defer func() { <-limit; conn.Close() }()
			conn.SetDeadline(time.Now().Add(10 * time.Second))
			x := s.currentSession()
			ip, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
			if x == nil || x.ctx.Err() != nil || !sameIP(ip, x.ip) {
				return
			}
			req, err := x.readRTSP(conn)
			if err != nil {
				log.Printf("gamestream: invalid RTSP request: %v", err)
				return
			}
			resp := x.handleRTSP(req)
			if err = x.writeRTSP(conn, resp); err != nil {
				log.Printf("gamestream: RTSP response: %v", err)
			}
		}()
	}
}

func (x *streamSession) handleRTSP(r rtspRequest) []byte {
	if x.ctx.Err() != nil {
		return rtspResponse(454, r.cseq, "", nil)
	}
	if sid := r.headers.Get("Session"); sid != "" && strings.TrimSpace(strings.Split(sid, ";")[0]) != x.id {
		return rtspResponse(454, r.cseq, "", nil)
	}
	log.Printf("gamestream: RTSP %s %s", r.method, r.target)
	extra := map[string]string{}
	body := ""
	code := 200
	switch r.method {
	case "OPTIONS":
		extra["Public"] = "OPTIONS, DESCRIBE, SETUP, ANNOUNCE, PLAY, TEARDOWN"
	case "DESCRIBE":
		extra["Content-Type"] = "application/sdp"
		body = "v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=AnyKVM\r\nt=0 0\r\na=x-ss-general.featureFlags:0\r\na=x-ss-general.encryptionSupported:7\r\na=x-ss-general.encryptionRequested:1\r\na=fmtp:97 surround-params=21101\r\na=fmtp:97 surround-params=21101\r\n"
		if x.server.supportsHEVC(x.machine) {
			// GameStream detects HEVC from this legacy GameStream SDP marker.
			// Real VPS/SPS/PPS are supplied with each recovery frame over video UDP.
			body += "a=fmtp:96 sprop-parameter-sets=AAAAAU\r\n"
		}
		if x.server.supportsAV1(x.machine) {
			body += "a=rtpmap:98 AV1/90000\r\n"
		}
	case "SETUP":
		kind := ""
		for _, v := range []string{"audio", "video", "control"} {
			if strings.Contains(r.target, "streamid="+v) {
				kind = v
			}
		}
		if kind == "" {
			code = 461
			break
		}
		x.mu.Lock()
		x.setup[kind] = true
		x.mu.Unlock()
		port := x.server.controlPort
		if kind == "video" {
			port = x.server.videoSocket.LocalAddr().(*net.UDPAddr).Port
		}
		if kind == "audio" {
			port = x.server.audioSocket.LocalAddr().(*net.UDPAddr).Port
		}
		extra["Transport"] = fmt.Sprintf("server_port=%d", port)
		extra["Session"] = x.id + ";timeout=90"
		if kind == "control" {
			extra["X-SS-Connect-Data"] = strconv.FormatUint(uint64(x.connectData), 10)
		} else {
			extra["X-SS-Ping-Payload"] = x.ping
		}
	case "ANNOUNCE":
		if err := x.announce(r.body); err != nil {
			log.Printf("gamestream: negotiation rejected: %v", err)
			code = 461
			x.stop("unsupported stream configuration")
		}
	case "PLAY":
		if err := x.play(); err != nil {
			log.Printf("gamestream: PLAY failed: %v", err)
			code = 455
		}
	case "TEARDOWN":
		x.stop("RTSP teardown")
	default:
		code = 501
	}
	return rtspResponse(code, r.cseq, body, extra)
}

func (x *streamSession) announce(data []byte) error {
	a := map[string]int{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\x00"))
		if !strings.HasPrefix(line, "a=") {
			continue
		}
		k, v, ok := strings.Cut(line[2:], ":")
		if !ok {
			continue
		}
		if _, exists := a[k]; exists {
			return errors.New("duplicate SDP attribute")
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			continue
		}
		a[k] = n
	}
	get := func(k string, fallback int) int {
		v, ok := a[k]
		if !ok {
			return fallback
		}
		return v
	}
	codec := "h264"
	switch get("x-nv-vqos[0].bitStreamFormat", 0) {
	case 0:
	case 1:
		if !x.server.supportsHEVC(x.machine) {
			return errors.New("HEVC is unavailable for this target")
		}
		codec = "hevc"
	case 2:
		if !x.server.supportsAV1(x.machine) {
			return errors.New("AV1 is unavailable for this target")
		}
		codec = "av1"
	default:
		return errors.New("unsupported video codec")
	}
	if get("x-nv-video[0].dynamicRangeMode", 0) != 0 || get("x-ss-video[0].chromaSamplingType", 0) != 0 {
		return errors.New("only SDR 8-bit 4:2:0 is supported")
	}
	if get("x-nv-general.useReliableUdp", 13) != 13 {
		return errors.New("encrypted control protocol 13 required")
	}
	if get("x-nv-audio.surround.numChannels", 2) != 2 {
		return errors.New("select Stereo audio in the GameStream client")
	}
	o := streamOptions{Codec: codec, Width: a["x-nv-video[0].clientViewportWd"], Height: a["x-nv-video[0].clientViewportHt"], FPS: a["x-nv-video[0].maxFPS"], Bitrate: get("x-nv-vqos[0].bw.maximumBitrateKbps", 10000), PacketSize: get("x-nv-video[0].packetSize", 1024), MinFEC: get("x-nv-vqos[0].fec.minRequiredFecPackets", 2), AudioDuration: get("x-nv-aqos.packetDuration", 5), Encryption: get("x-ss-general.encryptionEnabled", 0), Features: get("x-ml-general.featureFlags", 0)}
	if get("x-nv-general.featureFlags", 0)&0x20 != 0 {
		o.Encryption |= 4
	}
	if err := validateOptions(o); err != nil {
		return err
	}
	// Sunshine uses this client extension as the total bandwidth budget.
	// Without it, maximumBitrateKbps remains the legacy video-only value.
	configured := get("x-ml-video.configuredBitrateKbps", 0)
	if configured < 0 || configured > 150000 || (configured > 0 && configured < 100) {
		return errors.New("invalid configured bitrate")
	}
	if configured > 0 {
		o.Bitrate = videoBitrateBudget(configured)
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.started || x.announced {
		return errors.New("stream already configured")
	}
	x.options = o
	x.announced = true
	return nil
}

// Reserve FEC, actual Opus bandwidth, then packet/control overhead. Unlike
// Sunshine's audio quality modes, the encoder always sends 128 kbps stereo.
func videoBitrateBudget(configured int) int {
	video := configured * (100 - videoFECPercentage) / 100
	video -= min(opusBitrateKbps, video/5)
	video -= min(500, video/10)
	return max(100, video)
}
