package bridge

import "testing"

func TestRemoteControlActivityExcludesIdleAndLocalViewer(t *testing.T) {
	b := NewBridge(nil, "alt", 0, 0, 0)
	for _, tc := range []struct {
		proto string
		n     int
		want  bool
	}{{"", 0, false}, {"rdp", 0, false}, {"viewer", 1, false}, {"rdp", 1, true}, {"vnc", 1, true}, {"ws", 1, true}, {"gamestream", 1, true}} {
		b.activeProto, b.consumers = tc.proto, tc.n
		if got := b.RemoteControlActive(); got != tc.want {
			t.Fatalf("protocol=%q consumers=%d active=%v", tc.proto, tc.n, got)
		}
	}
}
