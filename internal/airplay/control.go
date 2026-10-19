package airplay

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
)

const maxControlBody = 64 << 10
const maxControlHeaders = 16 << 10
const maxFCUPActionBody = 8 << 20

type request struct {
	method, path, protocol string
	query                  url.Values
	headers                textproto.MIMEHeader
	body                   []byte
}

// net/http rejects RTSP/1.0. Parse both HTTP and RTSP on the same socket, with
// explicit budgets before allocation, including across pipelined requests.
func readRequest(r *bufio.Reader) (request, error) {
	var q request
	budget := maxControlHeaders
	line := func() (string, error) {
		data, err := r.ReadSlice('\n')
		budget -= len(data)
		if err != nil {
			return "", err
		}
		if budget < 0 || len(data) < 2 || data[len(data)-2] != '\r' {
			return "", fmt.Errorf("airplay: invalid/oversized control headers")
		}
		return string(data[:len(data)-2]), nil
	}
	first, err := line()
	if err != nil {
		return q, err
	}
	parts := strings.Split(first, " ")
	if len(parts) != 3 || (parts[2] != "RTSP/1.0" && parts[2] != "HTTP/1.1" && parts[2] != "HTTP/1.0") {
		return q, fmt.Errorf("airplay: invalid control request line")
	}
	q.method, q.protocol = parts[0], parts[2]
	target := parts[1]
	if rest, found := strings.CutPrefix(target, "rtsp://"); found {
		authority, path, _ := strings.Cut(rest, "/")
		// AirPlay senders emit unbracketed IPv6 RTSP authorities. The
		// authority never selects a destination: this socket owns the session.
		// Validate the address, then parse only the path instead of feeding
		// Apple's nonstandard authority into the generic URL port parser.
		if strings.Count(authority, ":") > 1 && !strings.HasPrefix(authority, "[") {
			address, _, _ := strings.Cut(authority, "%")
			if net.ParseIP(address) == nil {
				return q, fmt.Errorf("airplay: invalid IPv6 RTSP authority")
			}
			target = "/" + path
		}
	}
	u, err := url.Parse(target)
	if err != nil {
		return q, err
	}
	q.path = u.Path
	q.query = u.Query()
	q.headers = make(textproto.MIMEHeader)
	for {
		s, err := line()
		if err != nil {
			return q, err
		}
		if s == "" {
			break
		}
		key, value, ok := strings.Cut(s, ":")
		if !ok || key == "" || strings.ContainsAny(key, " \t") {
			return q, fmt.Errorf("airplay: invalid control header")
		}
		key = textproto.CanonicalMIMEHeaderKey(key)
		value = strings.TrimSpace(value)
		if (key == "Content-Length" || key == "Cseq") && len(q.headers[key]) > 0 {
			return q, fmt.Errorf("airplay: duplicate %s", key)
		}
		q.headers.Add(key, value)
	}
	if q.headers.Get("Transfer-Encoding") != "" {
		return q, fmt.Errorf("airplay: transfer encoding unsupported")
	}
	length := 0
	bodyLimit := maxControlBody
	if q.method == "POST" && q.path == "/action" && strings.HasPrefix(q.protocol, "HTTP/") {
		bodyLimit = maxFCUPActionBody
	}
	if s := q.headers.Get("Content-Length"); s != "" {
		length, err = strconv.Atoi(s)
		if err != nil || length < 0 || length > bodyLimit {
			return q, fmt.Errorf("airplay: invalid control body size")
		}
	}
	q.body = make([]byte, length)
	_, err = io.ReadFull(r, q.body)
	return q, err
}

func writeResponse(w io.Writer, q request, status int, body []byte, contentType string) error {
	reason := map[int]string{200: "OK", 400: "Bad Request", 403: "Forbidden", 404: "Not Found", 409: "Conflict", 453: "Not Enough Bandwidth", 461: "Unsupported Transport", 501: "Not Implemented", 503: "Service Unavailable"}[status]
	var b strings.Builder
	fmt.Fprintf(&b, "%s %d %s\r\nServer: AnyKVM-AirPlay\r\nContent-Length: %d\r\n", q.protocol, status, reason, len(body))
	if seq := q.headers.Get("Cseq"); seq != "" {
		fmt.Fprintf(&b, "CSeq: %s\r\n", seq)
	}
	if contentType != "" {
		fmt.Fprintf(&b, "Content-Type: %s\r\n", contentType)
	}
	if q.method == "OPTIONS" {
		b.WriteString("Public: OPTIONS, SETUP, RECORD, FLUSH, TEARDOWN, GET_PARAMETER, SET_PARAMETER\r\n")
	}
	if q.method == "RECORD" && q.protocol == "RTSP/1.0" && status == 200 {
		// Match /info's zero outputLatencyMicros: no receiver-side scheduled
		// playout delay is requested. Viewer buffering is consumer-dependent.
		b.WriteString("Audio-Latency: 0\r\nAudio-Jack-Status: connected; type=analog\r\n")
	}
	b.WriteString("\r\n")
	if _, err := io.WriteString(w, b.String()); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}
