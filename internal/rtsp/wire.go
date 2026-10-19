package rtsp

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

type response struct {
	status  int
	headers map[string]string
	body    []byte
}

func (s *session) line() (string, error) {
	p, err := s.r.ReadSlice('\n')
	if err != nil {
		return "", err
	}
	if len(p) > 8192 {
		return "", fmt.Errorf("RTSP header line too large")
	}
	return strings.TrimRight(string(p), "\r\n"), nil
}
func (s *session) read() (*response, []byte, byte, error) {
	first, err := s.r.Peek(1)
	if err != nil {
		return nil, nil, 0, err
	}
	if first[0] == '$' {
		var h [4]byte
		if _, err = io.ReadFull(s.r, h[:]); err != nil {
			return nil, nil, 0, err
		}
		p := make([]byte, int(binary.BigEndian.Uint16(h[2:])))
		_, err = io.ReadFull(s.r, p)
		return nil, p, h[1], err
	}
	line, err := s.line()
	if err != nil {
		return nil, nil, 0, err
	}
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "RTSP/1.0" {
		return nil, nil, 0, fmt.Errorf("invalid RTSP response")
	}
	status, err := strconv.Atoi(fields[1])
	if err != nil {
		return nil, nil, 0, fmt.Errorf("invalid RTSP status")
	}
	r := &response{status: status, headers: make(map[string]string)}
	total := len(line)
	for {
		line, err = s.line()
		if err != nil {
			return nil, nil, 0, err
		}
		total += len(line)
		if total > 65536 {
			return nil, nil, 0, fmt.Errorf("RTSP headers too large")
		}
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			return nil, nil, 0, fmt.Errorf("invalid RTSP header")
		}
		r.headers[strings.ToLower(k)] = textproto.TrimString(v)
	}
	if value := r.headers["content-length"]; value != "" {
		n, e := strconv.Atoi(value)
		if e != nil || n < 0 || n > 1<<20 {
			return nil, nil, 0, fmt.Errorf("RTSP body too large")
		}
		r.body = make([]byte, n)
		_, err = io.ReadFull(s.r, r.body)
	}
	return r, nil, 0, err
}
func (s *session) send(method, uri string, headers map[string]string, username string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.seq++
	auth, err := s.authorization(method, uri, username)
	if err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s RTSP/1.0\r\nCSeq: %d\r\nUser-Agent: AnyKVM\r\n", method, uri, s.seq)
	if s.session != "" {
		fmt.Fprintf(&b, "Session: %s\r\n", s.session)
	}
	if auth != "" {
		fmt.Fprintf(&b, "Authorization: %s\r\n", auth)
	}
	for k, v := range headers {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	b.WriteString("\r\n")
	s.conn.SetWriteDeadline(time.Now().Add(s.timeout))
	p := []byte(b.String())
	for len(p) > 0 {
		n, e := s.conn.Write(p)
		if e != nil {
			return e
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}
func (s *session) request(method, uri string, headers map[string]string, user string) (*response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		if err := s.send(method, uri, headers, user); err != nil {
			return nil, err
		}
		for {
			r, _, _, err := s.read()
			if err != nil {
				return nil, err
			}
			if r == nil {
				continue
			}
			if r.headers["cseq"] != strconv.Itoa(s.seq) {
				return nil, fmt.Errorf("RTSP response CSeq mismatch")
			}
			if r.status == 401 && attempt == 0 {
				s.auth = r.headers["www-authenticate"]
				break
			}
			if r.status/100 != 2 {
				return nil, fmt.Errorf("RTSP %s failed (status %d)", method, r.status)
			}
			return r, nil
		}
	}
	return nil, fmt.Errorf("RTSP authentication failed")
}
func digestFields(raw string) map[string]string {
	out := map[string]string{}
	for raw != "" {
		raw = strings.TrimLeft(raw, " ,\t")
		k, v, ok := strings.Cut(raw, "=")
		if !ok {
			break
		}
		k = strings.ToLower(strings.TrimSpace(k))
		raw = v
		var value string
		if strings.HasPrefix(raw, "\"") {
			raw = raw[1:]
			var b strings.Builder
			for len(raw) > 0 {
				c := raw[0]
				raw = raw[1:]
				if c == '"' {
					break
				}
				if c == '\\' && len(raw) > 0 {
					c = raw[0]
					raw = raw[1:]
				}
				b.WriteByte(c)
			}
			value = b.String()
		} else {
			value, raw, _ = strings.Cut(raw, ",")
		}
		out[k] = strings.TrimSpace(value)
	}
	return out
}
func (s *session) authorization(method, uri, user string) (string, error) {
	if s.auth == "" {
		return "", nil
	}
	scheme, params, _ := strings.Cut(s.auth, " ")
	if strings.EqualFold(scheme, "Basic") {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+s.password)), nil
	}
	if !strings.EqualFold(scheme, "Digest") {
		return "", fmt.Errorf("unsupported RTSP authentication scheme")
	}
	p := digestFields(params)
	if p["nonce"] == "" || p["realm"] == "" || (p["algorithm"] != "" && !strings.EqualFold(p["algorithm"], "MD5")) {
		return "", fmt.Errorf("unsupported RTSP Digest challenge")
	}
	hash := func(v string) string { h := md5.Sum([]byte(v)); return hex.EncodeToString(h[:]) }
	ha1 := hash(user + ":" + p["realm"] + ":" + s.password)
	ha2 := hash(method + ":" + uri)
	sum := hash(ha1 + ":" + p["nonce"] + ":" + ha2)
	quote := func(v string) string { return "\"" + strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(v) + "\"" }
	extra := ""
	if p["qop"] != "" {
		auth := false
		for _, q := range strings.Split(p["qop"], ",") {
			if strings.TrimSpace(q) == "auth" {
				auth = true
			}
		}
		if !auth {
			return "", fmt.Errorf("RTSP Digest requires qop auth")
		}
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return "", err
		}
		cnonce := hex.EncodeToString(nonce[:])
		s.nonceCount++
		nc := fmt.Sprintf("%08x", s.nonceCount)
		sum = hash(ha1 + ":" + p["nonce"] + ":" + nc + ":" + cnonce + ":auth:" + ha2)
		extra = ", qop=auth, nc=" + nc + ", cnonce=" + quote(cnonce)
	}
	result := "Digest username=" + quote(user) + ", realm=" + quote(p["realm"]) + ", nonce=" + quote(p["nonce"]) + ", uri=" + quote(uri) + ", response=" + quote(sum) + extra
	if p["opaque"] != "" {
		result += ", opaque=" + quote(p["opaque"])
	}
	return result, nil
}
