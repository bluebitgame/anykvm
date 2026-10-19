package coredevice

import (
	"context"
	"net"
	"strings"
	"testing"
)

func TestDisplayVersionRejectionReturnsError(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	wire := &inputXPCFixture{replies: []map[string]interface{}{
		{"CoreDevice.error": map[string]interface{}{"code": int64(9021), "description": "Remote control requires a newer iOS version"}},
	}}
	channel := &xpcChannel{x: wire, conn: a}
	output, err := channel.invoke(context.Background(), displayRequest("com.apple.coredevice.feature.startmediastream", "", nil))
	if output != nil || err == nil || !strings.Contains(err.Error(), "9021") || len(wire.messages) != 1 {
		t.Fatal("unsupported DisplayService was accepted or retried", output, err)
	}
}
