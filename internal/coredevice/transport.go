package coredevice

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/danielpaulus/go-ios/ios"
	ioshttp "github.com/danielpaulus/go-ios/ios/http"
)

const displayService = "com.apple.coredevice.displayservice"
const universalService = "com.apple.coredevice.hid.universalhidservice"
const legacyUniversalService = "com.apple.coredevice.hid.universalhid"

// ListUSBDevices returns selectors without opening developer services.
func ListUSBDevices() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	devices, err := listUSB(ctx)
	if err != nil {
		return nil, err
	}
	var selectors []string
	for _, device := range devices.DeviceList {
		if device.Properties.ConnectionType == "USB" {
			selectors = append(selectors, "coredevice:"+device.Properties.SerialNumber)
		}
	}
	sort.Strings(selectors)
	return selectors, nil
}

// endpoint owns only the tunnel it creates. An explicit RSD tunnel is borrowed.
type endpoint struct {
	address string
	ports   map[string]int
	udid    string
	tunnel  io.Closer
}

func (e *endpoint) Close() error {
	if e.tunnel != nil {
		return e.tunnel.Close()
	}
	return nil
}

func discover(ctx context.Context, udid string, o Options) (_ *endpoint, err error) {
	return discoverServices(ctx, udid, o, true)
}

func discoverServices(ctx context.Context, udid string, o Options, requireDisplay bool) (_ *endpoint, err error) {
	e := &endpoint{}
	defer func() {
		if err != nil {
			e.Close()
		}
	}()
	var rsdPort int
	if o.RSD != "" {
		host, port, _ := net.SplitHostPort(o.RSD)
		e.address = host
		rsdPort, _ = strconv.Atoi(port)
	} else {
		list, err := listUSB(ctx)
		if err != nil {
			return nil, fmt.Errorf("coredevice: USB enumeration: %w", err)
		}
		var candidates []ios.DeviceEntry
		for _, d := range list.DeviceList {
			if d.Properties.ConnectionType == "USB" && (udid == "" || d.Properties.SerialNumber == udid) {
				candidates = append(candidates, d)
			}
		}
		if len(candidates) != 1 {
			return nil, fmt.Errorf("coredevice: found %d matching USB devices; connect one or select coredevice:UDID", len(candidates))
		}
		e.address, rsdPort, e.tunnel, err = openUSBTunnel(ctx, candidates[0])
		if err != nil {
			return nil, fmt.Errorf("coredevice: USB tunnel (requires iOS 17.4+, trust and tunnel privileges): %w", err)
		}
	}
	c, err := dialXPC(ctx, net.JoinHostPort(e.address, strconv.Itoa(rsdPort)))
	if err != nil {
		return nil, fmt.Errorf("coredevice: RSD: %w", err)
	}
	defer c.Close()
	m, err := c.x.ReceiveOnClientServerStream()
	if err != nil {
		return nil, err
	}
	if m["MessageType"] != "Handshake" {
		return nil, fmt.Errorf("coredevice: invalid RSD handshake")
	}
	p, _ := m["Properties"].(map[string]interface{})
	e.udid, _ = p["UniqueDeviceID"].(string)
	if e.udid == "" || (udid != "" && udid != e.udid) {
		return nil, fmt.Errorf("coredevice: RSD device identity does not match %q", udid)
	}
	e.ports = make(map[string]int)
	services, _ := m["Services"].(map[string]interface{})
	for name, value := range services {
		entry, _ := value.(map[string]interface{})
		port, _ := entry["Port"].(string)
		n, _ := strconv.Atoi(port)
		if n > 0 && n <= 65535 {
			e.ports[name] = n
		}
	}
	if err := e.requireServices(requireDisplay); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *endpoint) requireServices(requireDisplay bool) error {
	if requireDisplay && e.ports[displayService] == 0 {
		return fmt.Errorf("coredevice: DisplayService unavailable; enable Developer Mode and mount a compatible Developer Disk Image, or configure video.source ios-usb with hid.device coredevice[:UDID] on macOS")
	}
	if e.ports[universalService] == 0 && e.ports[legacyUniversalService] == 0 {
		return fmt.Errorf("coredevice: Universal HID unavailable; enable Developer Mode and mount a compatible Developer Disk Image")
	}
	return nil
}

type xpcChannel struct {
	x          xpcProtocol
	conn       net.Conn
	cancel     func() bool
	hidFeature string
}

type xpcProtocol interface {
	Send(map[string]interface{}, ...uint32) error
	ReceiveOnClientServerStream() (map[string]interface{}, error)
	ReceiveOnServerClientStream() (map[string]interface{}, error)
}

func dialXPC(ctx context.Context, address string) (*xpcChannel, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	fail := func(e error) (*xpcChannel, error) { stop(); conn.Close(); return nil, e }
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	h, err := ioshttp.NewHttpConnection(conn)
	if err != nil {
		return fail(err)
	}
	x, err := ios.CreateXpcConnection(h)
	if err != nil {
		return fail(err)
	}
	return &xpcChannel{x: x, conn: conn, cancel: stop}, nil
}
func (c *xpcChannel) live()        { c.cancel(); c.conn.SetDeadline(time.Time{}) }
func (c *xpcChannel) Close() error { c.cancel(); return c.conn.Close() }
func (e *endpoint) connect(ctx context.Context, name string) (*xpcChannel, error) {
	port := e.ports[name]
	if port == 0 {
		return nil, fmt.Errorf("coredevice: missing service %s", name)
	}
	return dialXPC(ctx, net.JoinHostPort(e.address, strconv.Itoa(port)))
}

// The lockdown proxy carries whole IPv6 datagrams on a TCP byte stream. ReadFull
// preserves boundaries even when USB delivers a split or coalesced read.
type tunnelParameters struct {
	ClientParameters struct {
		Address string `json:"address"`
		MTU     int    `json:"mtu"`
	} `json:"clientParameters"`
	ServerAddress string `json:"serverAddress"`
	ServerRSDPort int    `json:"serverRSDPort"`
}

func handshakeTunnel(rw io.ReadWriter) (tunnelParameters, error) {
	body := []byte(`{"type":"clientHandshakeRequest","mtu":1280}`)
	request := append([]byte("CDTunnel\x00"), byte(len(body)))
	request = append(request, body...)
	if err := writeAll(rw, request); err != nil {
		return tunnelParameters{}, err
	}
	header := make([]byte, 10)
	if _, err := io.ReadFull(rw, header); err != nil {
		return tunnelParameters{}, err
	}
	if !bytes.Equal(header[:9], []byte("CDTunnel\x00")) || header[9] == 0 {
		return tunnelParameters{}, fmt.Errorf("invalid CoreDevice tunnel header")
	}
	body = make([]byte, int(header[9]))
	if _, err := io.ReadFull(rw, body); err != nil {
		return tunnelParameters{}, err
	}
	var p tunnelParameters
	if err := json.Unmarshal(body, &p); err != nil {
		return p, err
	}
	if net.ParseIP(p.ClientParameters.Address) == nil || net.ParseIP(p.ServerAddress) == nil || p.ServerRSDPort < 1 || p.ServerRSDPort > 65535 || p.ClientParameters.MTU < 1280 || p.ClientParameters.MTU > 65535 {
		return p, fmt.Errorf("invalid tunnel parameters")
	}
	return p, nil
}
func writeAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}
func forwardIPv6(dst io.Writer, src io.Reader) error {
	header := make([]byte, 40)
	for {
		if _, err := io.ReadFull(src, header); err != nil {
			return err
		}
		if header[0]>>4 != 6 {
			return fmt.Errorf("invalid IPv6 packet")
		}
		size := int(binary.BigEndian.Uint16(header[4:6]))
		if size == 0 {
			return fmt.Errorf("IPv6 jumbograms are unsupported")
		}
		p := make([]byte, 40+size)
		copy(p, header)
		if _, err := io.ReadFull(src, p[40:]); err != nil {
			return err
		}
		if err := writeAll(dst, p); err != nil {
			return err
		}
	}
}

type ownedTunnel struct {
	once sync.Once
	usb  io.Closer
	tun  io.Closer
}

func (t *ownedTunnel) Close() error { t.once.Do(func() { t.usb.Close(); t.tun.Close() }); return nil }
