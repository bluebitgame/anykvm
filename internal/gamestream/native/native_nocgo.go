//go:build !cgo

package native

import "errors"

type Host struct{}
type Event struct {
	Type              int
	Peer, ConnectData uint32
	Address           string
	Data              []byte
	Channel           uint8
}

const (
	Connect    = 1
	Disconnect = 2
	Receive    = 3
)

var unavailable = errors.New("GameStream requires CGO_ENABLED=1 (bundled ENet/FEC)")

func NewHost(string, int) (*Host, error)                              { return nil, unavailable }
func (*Host) Port() int                                               { return 0 }
func (*Host) Close()                                                  {}
func (*Host) Poll(int) (Event, error)                                 { return Event{}, unavailable }
func (*Host) Send(uint32, []byte) error                               { return unavailable }
func (*Host) Reset(uint32)                                            {}
func (*Host) Dial(string, int, uint32) error                          { return unavailable }
func Code([]byte, int, int, int, bool, []byte) error                  { return unavailable }
func CodeStrided([]byte, int, int, int, int, int, bool, []byte) error { return unavailable }

func (*Host) Stats(uint32) (PeerStats, bool) { return PeerStats{}, false }
