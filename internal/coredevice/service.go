package coredevice

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"net"
	"time"

	"github.com/danielpaulus/go-ios/ios/xpc"
	"github.com/google/uuid"
)

type streamConfig struct {
	sourcePort            int
	localSSRC, remoteSSRC uint32
	payloadType           byte
}

func (c *xpcChannel) startAudioStream(ctx context.Context, e *endpoint, u *net.UDPConn, id uuid.UUID) (streamConfig, error) {
	local := u.LocalAddr().(*net.UDPAddr)
	offer, err := audioOffer(uuid.New(), rand.Uint32())
	if err != nil {
		return streamConfig{}, err
	}
	output, err := c.invoke(ctx, displayRequest("com.apple.coredevice.feature.startmediastream", "com.apple.coredevice.action.mediastreamstart", map[string]interface{}{
		"clientSupportedFeatures": uint64(140), "direction": "output", "negotiatorOffer": offer,
		"options": map[string]interface{}{
			"AVCMediaStreamNegotiatorAccessNetworkType":     map[string]interface{}{"int": int64(1)},
			"AVCMediaStreamNegotiatorTransportProtocolType": map[string]interface{}{"int": int64(2)},
			"avcMediaStreamOptionClientSessionID":           map[string]interface{}{"uuid": id},
		}, "receiverIP": local.IP.String(), "receiverPort": uint64(local.Port), "senderIP": e.address, "timeout": uint64(10), "type": "audio",
	}))
	if err != nil {
		return streamConfig{}, fmt.Errorf("DisplayService audio negotiation: %w", err)
	}
	connection, _ := output["connection"].(map[string]interface{})
	cfg, _ := connection["streamConfig"].(map[string]interface{})
	return audioStreamConfig(cfg)
}
func audioStreamConfig(cfg map[string]interface{}) (streamConfig, error) {
	pt := number(cfg["RxPayloadType"])
	result := streamConfig{sourcePort: int(number(cfg["SourcePort"])), localSSRC: uint32(number(cfg["RemoteSSRC"])), remoteSSRC: uint32(number(cfg["LocalSSRC"])), payloadType: byte(pt)}
	if number(cfg["AudioStreamMode"]) != 8 || pt != 101 || result.sourcePort < 1 || result.sourcePort > 65535 || result.localSSRC == 0 || result.remoteSSRC == 0 {
		return streamConfig{}, fmt.Errorf("coredevice: unsupported audio stream config: mode=%d PT=%d port=%d", number(cfg["AudioStreamMode"]), pt, result.sourcePort)
	}
	return result, nil
}

func number(v interface{}) uint64 {
	switch n := v.(type) {
	case uint64:
		return n
	case int64:
		if n > 0 {
			return uint64(n)
		}
	case int:
		return uint64(max(n, 0))
	case uint32:
		return uint64(n)
	}
	return 0
}
func displayRequest(feature, action string, input map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"CoreDevice.CoreDeviceDDIProtocolVersion": int64(2), "CoreDevice.action": map[string]interface{}{}, "CoreDevice.actionIdentifier": action,
		"CoreDevice.coreDeviceVersion": map[string]interface{}{"components": []interface{}{uint64(629), uint64(3)}, "originalComponentsCount": int64(2), "stringValue": "629.3"},
		"CoreDevice.deviceIdentifier":  uuid.New().String(), "CoreDevice.featureIdentifier": feature, "CoreDevice.input": input, "CoreDevice.invocationIdentifier": uuid.New().String(),
	}
}
func (c *xpcChannel) invoke(ctx context.Context, request map[string]interface{}) (map[string]interface{}, error) {
	stop := context.AfterFunc(ctx, func() { c.conn.Close() })
	defer stop()
	if d, ok := ctx.Deadline(); ok {
		c.conn.SetDeadline(d)
	}
	defer c.conn.SetDeadline(time.Time{})
	if err := c.x.Send(request, xpc.HeartbeatRequestFlag); err != nil {
		return nil, err
	}
	response, err := c.x.ReceiveOnServerClientStream()
	if err != nil {
		return nil, err
	}
	output, ok := response["CoreDevice.output"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("coredevice: service rejected request: %v", response)
	}
	return output, nil
}
func (c *xpcChannel) startStream(ctx context.Context, e *endpoint, u *net.UDPConn, id uuid.UUID) (streamConfig, error) {
	local := u.LocalAddr().(*net.UDPAddr)
	offer, err := videoOffer(uuid.New(), rand.Uint32())
	if err != nil {
		return streamConfig{}, err
	}
	output, err := c.invoke(ctx, displayRequest("com.apple.coredevice.feature.startmediastream", "com.apple.coredevice.action.mediastreamstart", map[string]interface{}{
		"clientSupportedFeatures": uint64(140), "direction": "output", "negotiatorOffer": offer,
		"options": map[string]interface{}{
			"AVCMediaStreamNegotiatorAccessNetworkType":     map[string]interface{}{"int": int64(1)},
			"AVCMediaStreamNegotiatorTransportProtocolType": map[string]interface{}{"int": int64(2)},
			"CoreDeviceVideoDisplayMode":                    map[string]interface{}{"string": "DisplayByID"}, "VideoStreamForDisplayID": map[string]interface{}{"int": int64(1)},
			"avcMediaStreamOptionClientSessionID": map[string]interface{}{"uuid": id},
		}, "receiverIP": local.IP.String(), "receiverPort": uint64(local.Port), "senderIP": e.address, "timeout": uint64(10), "type": "video",
	}))
	if err != nil {
		return streamConfig{}, fmt.Errorf("DisplayService negotiation: %w", err)
	}
	connection, _ := output["connection"].(map[string]interface{})
	cfg, _ := connection["streamConfig"].(map[string]interface{})
	result := streamConfig{sourcePort: int(number(cfg["SourcePort"])), localSSRC: uint32(number(cfg["RemoteSSRC"])), remoteSSRC: uint32(number(cfg["LocalSSRC"]))}
	if result.sourcePort < 1 || result.sourcePort > 65535 || result.localSSRC == 0 || result.remoteSSRC == 0 {
		return result, fmt.Errorf("coredevice: missing RTCP endpoints in streamConfig")
	}
	return result, nil
}
func (c *xpcChannel) stopStream(id uuid.UUID) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := c.invoke(ctx, displayRequest("com.apple.coredevice.feature.stopmediastream", "com.apple.coredevice.action.mediastreamstop", map[string]interface{}{
		"avcMediaStreamOptionClientSessionID": map[string]interface{}{"uuid": id}, "stopAll": true,
	}))
	return err
}

// A cancelled negotiation may have started streaming before its reply was
// received. The old channel is then unusable; stop using the same session UUID
// on a fresh connection before releasing our tunnel.
func (e *endpoint) stopStream(channel *xpcChannel, id uuid.UUID) {
	if err := channel.stopStream(id); err == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	fresh, err := e.connect(ctx, displayService)
	if err != nil {
		return
	}
	defer fresh.Close()
	_ = fresh.stopStream(id)
}
func openReceiver(ctx context.Context, address string) (*net.UDPConn, error) {
	route, err := (&net.Dialer{}).DialContext(ctx, "udp6", net.JoinHostPort(address, "9"))
	if err != nil {
		return nil, err
	}
	local := route.LocalAddr().(*net.UDPAddr)
	route.Close()
	u, err := net.ListenUDP("udp6", &net.UDPAddr{IP: local.IP, Zone: local.Zone})
	if err != nil {
		return nil, err
	}
	_ = u.SetReadBuffer(4 << 20)
	return u, nil
}
func (cfg streamConfig) feedback(highest uint32, pli bool) []byte {
	p := make([]byte, 44)
	p[0], p[1], p[3] = 0x81, 201, 7
	binary.BigEndian.PutUint32(p[4:], cfg.localSSRC)
	binary.BigEndian.PutUint32(p[8:], cfg.remoteSSRC)
	binary.BigEndian.PutUint32(p[16:], highest)
	p[32], p[33], p[35] = 0x81, 202, 2
	binary.BigEndian.PutUint32(p[36:], cfg.localSSRC)
	p[40] = 1
	if pli {
		q := make([]byte, 12)
		q[0], q[1], q[3] = 0x81, 206, 2
		binary.BigEndian.PutUint32(q[4:], cfg.localSSRC)
		binary.BigEndian.PutUint32(q[8:], cfg.remoteSSRC)
		p = append(p, q...)
	}
	return p
}
