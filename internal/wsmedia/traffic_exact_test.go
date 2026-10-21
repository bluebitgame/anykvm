package wsmedia

import (
	"anykvm/internal/netstats"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type trafficReadConn struct {
	net.Conn
	received *atomic.Uint64
}

func (c *trafficReadConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.received.Add(uint64(n))
	return n, err
}

func TestUplinkMatchesSocketBytesExactlyWithTLSAndHijack(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprint(secure), func(t *testing.T) {
			before := netstats.Total()
			s := NewServer(Config{})
			written := make(chan uint64, 1)
			release := make(chan struct{})
			handlerDone := make(chan struct{})
			ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(handlerDone)
				conn, err := s.upgrader.Upgrade(w, r, nil)
				if err != nil {
					written <- 0
					return
				}
				c := &wsClient{conn: conn, done: make(chan struct{})}
				defer c.close()
				for _, msg := range []*outboundMessage{
					{header: []byte{msgMetadata}, data: make([]byte, 50)},
					{header: []byte{msgPCM}, data: make([]byte, 5000)},
					{header: []byte{msgHEVC}, data: make([]byte, 8192)},
				} {
					if !c.writeOutbound(msg) {
						written <- 0
						return
					}
				}
				written <- netstats.Total() - before
				<-release
			}))
			ts.Listener = netstats.WrapListener(ts.Listener)
			if secure {
				ts.StartTLS()
			} else {
				ts.Start()
			}
			defer ts.Close()
			defer func() { close(release); <-handlerDone }()
			var received atomic.Uint64
			dialer := websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				return &trafficReadConn{Conn: conn, received: &received}, nil
			}}
			conn, _, err := dialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			for range 3 {
				if _, _, err = conn.ReadMessage(); err != nil {
					t.Fatal(err)
				}
			}
			if total := <-written; total == 0 || total != received.Load() {
				t.Fatalf("counter=%d peer socket received=%d (double count or omission)", total, received.Load())
			}
		})
	}
}

func TestUplinkAggregatesSeparateHTTPConnections(t *testing.T) {
	before := netstats.Total()
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(make([]byte, 1024)) }))
	ts.Listener = netstats.WrapListener(ts.Listener)
	ts.Start()
	defer ts.Close()
	var received uint64
	for range 2 {
		conn, err := net.DialTimeout("tcp", ts.Listener.Addr().String(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		_, err = fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: test\r\nConnection: close\r\n\r\n")
		if err != nil {
			conn.Close()
			t.Fatal(err)
		}
		body, err := io.ReadAll(conn)
		conn.Close()
		if err != nil {
			t.Fatal(err)
		}
		received += uint64(len(body))
	}
	if n := netstats.Total() - before; n != received {
		t.Fatalf("HTTP sent=%d actual bytes=%d", n, received)
	}
}
