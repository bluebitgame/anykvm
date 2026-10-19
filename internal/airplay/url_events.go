package airplay

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"howett.net/plist"
)

type reverseEvents struct {
	key      playbackKey
	conn     net.Conn
	states   chan string
	requests chan reverseRequest
	done     chan struct{}
}

type reverseRequest struct {
	ctx  context.Context
	body []byte
}

func (r *Receiver) sendMediaEvent(key playbackKey, state string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.urlEvents
	if e == nil || e.key != key {
		return
	}
	select {
	case e.states <- state:
	default:
		// Keep the newest state when a sender stops reading its reverse connection.
		select {
		case <-e.states:
		default:
		}
		select {
		case e.states <- state:
		default:
		}
	}
}
func readEventAck(reader *bufio.Reader) error {
	// Bound each acknowledgement, including headers, before net/http allocates.
	response, err := http.ReadResponse(bufio.NewReader(io.LimitReader(reader, maxControlHeaders+maxControlBody)), nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.ContentLength < 0 || response.ContentLength > maxControlBody || response.StatusCode != 200 {
		return fmt.Errorf("invalid AirPlay event acknowledgement")
	}
	_, err = io.Copy(io.Discard, io.LimitReader(response.Body, maxControlBody+1))
	return err
}

// /reverse dedicates this socket to receiver-originated HTTP requests. It is
// independent of short-lived /play and /rate connections from the same sender.
func (r *Receiver) serveReverse(q request, conn net.Conn, reader *bufio.Reader) {
	key, err := mediaKey(q, conn)
	if err != nil || q.protocol != "HTTP/1.1" || !strings.EqualFold(q.headers.Get("Upgrade"), "PTTH/1.0") || !strings.Contains(strings.ToLower(q.headers.Get("Connection")), "upgrade") {
		log.Printf("airplay: POST /reverse from %s -> 400", conn.RemoteAddr())
		writeResponse(conn, q, 400, nil, "")
		return
	}
	e := &reverseEvents{key: key, conn: conn, states: make(chan string, 8), requests: make(chan reverseRequest, 1), done: make(chan struct{})}
	defer close(e.done)
	r.mu.Lock()
	if r.urlPlayer != nil && r.urlPlayer.key != key || r.owner != nil && r.urlPlayer == nil && !samePlaybackPeer(r.owner, key) {
		r.mu.Unlock()
		log.Printf("airplay: POST /reverse from %s -> 453 (receiver owned by another sender/session)", conn.RemoteAddr())
		writeResponse(conn, q, 453, nil, "")
		return
	}
	old := r.urlEvents
	r.urlEvents = e
	p, stop := r.urlPlayer, r.stop
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		if r.urlEvents == e {
			r.urlEvents = nil
		}
		r.mu.Unlock()
	}()
	if old != nil {
		old.conn.Close()
	}
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: PTTH/1.0\r\nConnection: Upgrade\r\n\r\n"); err != nil {
		log.Printf("airplay: POST /reverse from %s upgrade failed: %v", conn.RemoteAddr(), err)
		return
	}
	log.Printf("airplay: POST /reverse from %s -> 101", conn.RemoteAddr())
	// Read continuously so EOF also releases idle or replaced reverse sockets.
	// This worker exits before serveReverse returns; no orphaned socket readers.
	acks := make(chan error)
	readerDone := make(chan struct{})
	quit := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			err := readEventAck(reader)
			select {
			case acks <- err:
			case <-quit:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	defer func() { close(quit); conn.Close(); <-readerDone }()
	if p != nil && p.key == key {
		p.mu.Lock()
		state := p.state
		p.mu.Unlock()
		e.states <- state
	}
	// A reverse socket often arrives before /play. Do not announce that an
	// unstarted video has stopped; wait for playback or FCUP resource events.
	for {
		var body []byte
		select {
		case <-stop:
			return
		case <-acks:
			return // EOF or unsolicited response while no event is pending.
		case state := <-e.states:
			body, _ = plist.Marshal(map[string]any{"category": "video", "sessionID": uint64(1), "state": state}, plist.XMLFormat)
		case event := <-e.requests:
			if event.ctx.Err() != nil {
				continue
			}
			body = event.body
		}
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := fmt.Fprintf(conn, "POST /event HTTP/1.1\r\nContent-Type: text/x-apple-plist+xml\r\nContent-Length: %d\r\nX-Apple-Session-ID: %s\r\n\r\n", len(body), key.session); err != nil {
			return
		}
		if _, err := conn.Write(body); err != nil {
			return
		}
		select {
		case err := <-acks:
			if err != nil {
				return
			}
		case <-stop:
			return
		}
		conn.SetDeadline(time.Time{})
	}
}
