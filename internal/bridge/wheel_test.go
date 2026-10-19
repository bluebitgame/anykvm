package bridge

import (
	"reflect"
	"testing"
)

type wheelHID struct {
	HIDSender
	deltas []int
}

func (h *wheelHID) MouseWheel(delta int) error {
	h.deltas = append(h.deltas, delta)
	return nil
}

func TestTargetWheelScale(t *testing.T) {
	for _, tc := range []struct {
		name        string
		scale       float64
		input, want []int
	}{
		{"normal", 1, []int{1, -2}, []int{1, -2}},
		{"amplified", 2, []int{1, -2}, []int{2, -4}},
		{"reversed", -2, []int{1, -2}, []int{-2, 4}},
		{"fractional", .5, []int{1, 1, -1, -1}, []int{1, -1}},
		{"fractional-reversed", -.5, []int{1, 1, -1, -1}, []int{-1, 1}},
		{"decimal", .1, []int{1, 1, 1, 1, 1, 1, 1, 1, 1, 1}, []int{1}},
		{"disabled", 0, []int{1, -2}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &wheelHID{}
			b := NewBridge([]*MachineSession{{HID: h, WheelScale: tc.scale}}, "alt", 0, 0, 0)
			for _, delta := range tc.input {
				if err := b.MouseWheel(delta); err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(h.deltas, tc.want) {
				t.Fatalf("deltas=%v, want %v", h.deltas, tc.want)
			}
		})
	}
}

func TestWheelRemainderClearedWhenControlEnds(t *testing.T) {
	h := &wheelHID{}
	m := &MachineSession{HID: h, WheelScale: .5}
	b := NewBridge([]*MachineSession{m}, "alt", 0, 0, 0)
	_ = b.MouseWheel(1)
	stopTargetHID(m)
	_ = b.MouseWheel(1)
	if len(h.deltas) != 0 {
		t.Fatal("wheel remainder carried into next session")
	}
	_ = b.MouseWheel(1)
	if !reflect.DeepEqual(h.deltas, []int{1}) {
		t.Fatal("new session did not accumulate its own wheel events")
	}
}
