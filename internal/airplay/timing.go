package airplay

import (
	"encoding/binary"
	"net"
	"sync"
	"time"
)

const ntpEpochMicros int64 = 2208988800 * 1000000

// AirPlay timing replies carry an NTP epoch adjustment, while mirror headers
// contain the sender's uptime clock. Keep that difference out of frame users.
type timingSession struct {
	conn      *net.UDPConn
	stop      chan struct{}
	wg        sync.WaitGroup
	mu        sync.Mutex
	offset    int64
	synced    bool
	bestDelay int64
}

func newTimingSession(local, peer *net.TCPAddr, port uint16) (*timingSession, error) {
	conn, err := net.DialUDP("udp", &net.UDPAddr{IP: local.IP, Zone: local.Zone}, &net.UDPAddr{IP: peer.IP, Zone: peer.Zone, Port: int(port)})
	if err != nil {
		return nil, err
	}
	t := &timingSession{conn: conn, stop: make(chan struct{})}
	t.wg.Add(1)
	go t.run()
	return t, nil
}

func (t *timingSession) port() int { return t.conn.LocalAddr().(*net.UDPAddr).Port }

func ntpMicros(raw uint64) int64 {
	return int64(raw>>32)*1000000 + int64((raw&0xffffffff)*1000000>>32)
}

func ntpTimestamp(now time.Time) uint64 {
	micros := now.UnixMicro() + ntpEpochMicros
	return uint64(micros/1000000)<<32 | uint64(micros%1000000)*(1<<32)/1000000
}

func (t *timingSession) run() {
	defer t.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-t.stop:
			return
		default:
		}
		var request [32]byte
		request[0], request[1], request[3] = 0x80, 0xd2, 7
		sent := time.Now()
		stamp := ntpTimestamp(sent)
		binary.BigEndian.PutUint64(request[24:], stamp)
		t.conn.SetDeadline(sent.Add(300 * time.Millisecond))
		if _, err := t.conn.Write(request[:]); err == nil {
			var response [64]byte
			n, err := t.conn.Read(response[:])
			arrived := time.Now()
			if err == nil && n == 32 && response[1]&0x7f == 0x53 && binary.BigEndian.Uint64(response[8:16]) == stamp {
				remote1 := ntpMicros(binary.BigEndian.Uint64(response[16:24])) - ntpEpochMicros
				remote2 := ntpMicros(binary.BigEndian.Uint64(response[24:32])) - ntpEpochMicros
				delay := (arrived.UnixMicro() - sent.UnixMicro()) - (remote2 - remote1)
				if delay >= 0 && delay <= 300000 && remote2 >= remote1 {
					t.mu.Lock()
					if !t.synced || delay < t.bestDelay {
						t.offset = ((remote1 - sent.UnixMicro()) + (remote2 - arrived.UnixMicro())) / 2
						t.synced, t.bestDelay = true, delay
					}
					t.mu.Unlock()
				}
			}
		}
		select {
		case <-t.stop:
			return
		case <-ticker.C:
		}
	}
}

func (t *timingSession) presentationTime(stamp uint64, arrived time.Time) time.Time {
	t.mu.Lock()
	offset, synced := t.offset, t.synced
	t.mu.Unlock()
	if synced && stamp != 0 {
		mapped := time.UnixMicro(ntpMicros(stamp) - offset)
		if delta := arrived.Sub(mapped); delta >= -time.Second && delta <= 10*time.Second {
			return mapped
		}
	}
	// No clock mapping yet: use the earliest full-packet arrival time.
	return arrived
}

func (t *timingSession) Close() {
	close(t.stop)
	t.conn.Close()
	t.wg.Wait()
}
