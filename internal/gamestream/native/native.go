//go:build cgo

// Package native wraps the exact ENet wire implementation used by GameStream
// and nanors Reed-Solomon coding. Hosts must only be used from one goroutine.
package native

/*
#cgo CFLAGS: -I${SRCDIR}/third_party/enet/include -I${SRCDIR}/third_party/nanors -I${SRCDIR}/third_party/nanors/deps/obl
#cgo !windows CFLAGS: -DHAS_SOCKLEN_T=1 -DHAS_GETADDRINFO=1 -DHAS_GETNAMEINFO=1 -DHAS_INET_PTON=1 -DHAS_INET_NTOP=1 -DHAS_MSGHDR_FLAGS=1 -DHAS_FCNTL=1
#cgo windows LDFLAGS: -lws2_32 -lwinmm -lqwave
#include "wrapper.h"
*/
import "C"

import (
	"errors"
	"sync"
	"unsafe"
)

var once sync.Once
var initErr error

type Host struct{ ptr *C.ENetHost }
type Event struct {
	Type        int
	Peer        uint32
	ConnectData uint32
	Address     string
	Data        []byte
	Channel     uint8
}

const (
	Connect    = 1
	Disconnect = 2
	Receive    = 3
)

func initialize() error {
	once.Do(func() {
		if C.enet_initialize() != 0 {
			initErr = errors.New("ENet initialization failed")
		}
		C.reed_solomon_init()
	})
	return initErr
}

func NewHost(ip string, port int) (*Host, error) {
	if err := initialize(); err != nil {
		return nil, err
	}
	s := C.CString(ip)
	defer C.free(unsafe.Pointer(s))
	h := C.bk_host(s, C.int(port))
	if h == nil {
		return nil, errors.New("ENet bind failed")
	}
	return &Host{h}, nil
}
func (h *Host) Port() int { return int(C.bk_port(h.ptr)) }
func (h *Host) Close() {
	if h.ptr != nil {
		C.enet_host_destroy(h.ptr)
		h.ptr = nil
	}
}
func (h *Host) Poll(ms int) (Event, error) {
	var e C.ENetEvent
	n := C.enet_host_service(h.ptr, &e, C.enet_uint32(ms))
	if n < 0 {
		return Event{}, errors.New("ENet service failed")
	}
	if n == 0 {
		return Event{}, nil
	}
	var addr [128]C.char
	C.bk_address(e.peer, &addr[0], 128)
	v := Event{Type: int(e._type), Peer: uint32(e.peer.incomingPeerID), ConnectData: uint32(e.data), Address: C.GoString(&addr[0]), Channel: uint8(e.channelID)}
	if e.packet != nil {
		v.Data = C.GoBytes(unsafe.Pointer(e.packet.data), C.int(e.packet.dataLength))
		C.enet_packet_destroy(e.packet)
	}
	return v, nil
}
func (h *Host) Send(peer uint32, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if C.bk_send(h.ptr, C.uint(peer), unsafe.Pointer(&data[0]), C.size_t(len(data))) != 0 {
		return errors.New("ENet send failed")
	}
	return nil
}
func (h *Host) Reset(peer uint32) { C.bk_reset(h.ptr, C.uint(peer)) }

// Dial is used by protocol integration tests, and is subject to the same
// single-goroutine ownership as Poll/Send.
func (h *Host) Dial(ip string, port int, data uint32) error {
	s := C.CString(ip)
	defer C.free(unsafe.Pointer(s))
	if C.bk_connect(h.ptr, s, C.int(port), C.uint(data)) != 0 {
		return errors.New("ENet connect failed")
	}
	return nil
}

// Code updates parity (decode=false) or reconstructs missing shards in place.
// Shards occupy contiguous, equally sized regions; no Go pointers enter C.
func Code(shards []byte, data, parity, size int, audio bool, missing []byte) error {
	return CodeStrided(shards, data, parity, size, size, 0, audio, missing)
}

// CodeStrided leaves a prefix/tail around each shard untouched. Video encryption
// uses these gaps for its IV/header/tag so FEC and AES-GCM share final storage.
func CodeStrided(shards []byte, data, parity, size, stride, offset int, audio bool, missing []byte) error {
	if data < 1 || data > 254 || parity < 1 || parity > 254 || data+parity > 255 || size < 1 || stride < size || stride > 2147483647 || offset < 0 || offset > stride-size || len(shards)/stride != data+parity || len(shards)%stride != 0 {
		return errors.New("invalid FEC dimensions")
	}
	if len(missing) != 0 && len(missing) != data+parity {
		return errors.New("invalid FEC marks")
	}
	if err := initialize(); err != nil {
		return err
	}
	var marks *C.uchar
	if len(missing) > 0 {
		marks = (*C.uchar)(unsafe.Pointer(&missing[0]))
	}
	a := 0
	if audio {
		if data != 4 || parity != 2 {
			return errors.New("invalid audio FEC dimensions")
		}
		a = 1
	}
	if C.bk_fec((*C.uchar)(unsafe.Pointer(&shards[offset])), C.int(data), C.int(parity), C.int(size), C.int(stride), C.int(a), marks) != 0 {
		return errors.New("FEC coding failed")
	}
	return nil
}

// Stats must be called by the same goroutine that owns Poll/Send.
func (h *Host) Stats(peer uint32) (PeerStats, bool) {
	var rtt, variation, loss C.uint
	if C.bk_peer_stats(h.ptr, C.uint(peer), &rtt, &variation, &loss) == 0 {
		return PeerStats{}, false
	}
	return PeerStats{uint32(rtt), uint32(variation), float64(loss) * 100 / float64(C.ENET_PEER_PACKET_LOSS_SCALE)}, true
}
