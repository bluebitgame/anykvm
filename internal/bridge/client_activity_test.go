package bridge

import (
	"reflect"
	"testing"

	"anykvm/internal/capture"
)

func TestClientActivityAcrossProtocols(t *testing.T) {
	b := NewBridge([]*MachineSession{{Name: "No signal", FallbackW: 320, FallbackH: 240}}, "alt", 30, 0, 0)
	var activity []bool
	b.SetClientActivityHandler(func(active bool) { activity = append(activity, active) })
	b.SetWS(&takeoverServer{close: b.OnClientDisconnected}, nil)
	b.SetGameStream(&takeoverServer{close: b.StopGameStream}, nil)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	if len(activity) != 0 {
		t.Fatal("server startup must not mark a client active")
	}
	b.ClaimWS()
	b.OnClientConnected()
	if err := b.StartGameStream(0, capture.VideoStreamOptions{Width: 640, Height: 480, FPS: 30}); err != nil {
		t.Fatal(err)
	}
	b.StartRDP()
	if !reflect.DeepEqual(activity, []bool{true}) {
		t.Fatalf("protocol takeovers released an active consumer: %v", activity)
	}
	b.OnClientDisconnected()
	b.OnClientDisconnected() // Duplicate cleanup must not emit another inactive event.
	if !reflect.DeepEqual(activity, []bool{true, false}) {
		t.Fatalf("connection activity: %v", activity)
	}
}

func TestClientActivityMultipleConsumersAndShutdown(t *testing.T) {
	b := NewBridge(nil, "alt", 30, 0, 0)
	var activity []bool
	b.SetClientActivityHandler(func(active bool) { activity = append(activity, active) })
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	b.OnClientConnected()
	b.OnClientConnected()
	b.OnClientDisconnected()
	if !reflect.DeepEqual(activity, []bool{true}) {
		t.Fatalf("disconnected while another consumer remained: %v", activity)
	}
	b.Stop()
	if !reflect.DeepEqual(activity, []bool{true, false}) {
		t.Fatalf("shutdown must release active consumers: %v", activity)
	}
}

func TestRejectedGameStreamDoesNotMarkClientActive(t *testing.T) {
	b := NewBridge(nil, "alt", 30, 0, 0)
	b.SetClientActivityHandler(func(bool) { t.Fatal("rejected stream marked a client active") })
	if err := b.StartGameStream(0, capture.VideoStreamOptions{}); err == nil {
		t.Fatal("expected an invalid-machine error")
	}
}
