package coredevice

import (
	"encoding/binary"
	"net"
	"testing"

	"anykvm/internal/capture"
)

func rotationInputFixture(t *testing.T, standalone bool) (*Client, *recordingXPC) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	wire := &recordingXPC{}
	c, err := NewClient("coredevice", Options{})
	if err != nil {
		t.Fatal(err)
	}
	c.inputOnly = standalone
	c.session = &session{client: c, input: &inputState{channel: &xpcChannel{x: wire, conn: a, cancel: func() bool { return true }}}}
	return c, wire
}

func assertTouchPosition(t *testing.T, report []byte, down bool, x, y int) {
	t.Helper()
	wantState := byte(2)
	if down {
		wantState = 0xc2
	}
	if report[3] != wantState || int(binary.LittleEndian.Uint16(report[4:])) != x*65535/32767 || int(binary.LittleEndian.Uint16(report[6:])) != y*65535/32767 {
		t.Fatalf("touch=%x want down=%v position=(%d,%d)", report, down, x, y)
	}
}

func TestStandaloneTouchRotation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		orientation int32
		w, h        int
		corners     [4][2]int
		point       [2]int
	}{
		{"portrait", 1, 1328, 2896, [4][2]int{{0, 0}, {32767, 0}, {32767, 32767}, {0, 32767}}, [2]int{8192, 24575}},
		{"upside-down", 2, 1328, 2896, [4][2]int{{32767, 32767}, {0, 32767}, {0, 0}, {32767, 0}}, [2]int{24575, 8192}},
		{"landscape-right", 3, 2896, 1328, [4][2]int{{32767, 0}, {32767, 32767}, {0, 32767}, {0, 0}}, [2]int{8192, 8192}},
		{"landscape-left", 4, 2896, 1328, [4][2]int{{0, 32767}, {0, 0}, {32767, 0}, {32767, 32767}}, [2]int{24575, 24575}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, wire := rotationInputFixture(t, true)
			c.session.setOrientation(tc.orientation)
			c.SetInputDimensions(tc.w, tc.h)
			points := [][2]int{{0, 0}, {32767, 0}, {32767, 32767}, {0, 32767}, {8192, 24575}}
			for j, point := range points {
				if err := c.MouseMove(point[0], point[1]); err != nil {
					t.Fatal(err)
				}
				if err := c.MouseButton(1, true); err != nil {
					t.Fatal(err)
				}
				want := tc.point
				if j < 4 {
					want = tc.corners[j]
				}
				assertTouchPosition(t, wire.reports[len(wire.reports)-1], true, want[0], want[1])
				if err := c.MouseButton(1, false); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestStandaloneRotationWaitsForMatchingVideoAndReleasesDrag(t *testing.T) {
	for _, orientationFirst := range []bool{true, false} {
		c, wire := rotationInputFixture(t, true)
		c.SetInputDimensions(1328, 2896)
		c.session.setOrientation(1)
		_ = c.MouseMove(8192, 24575)
		_ = c.MouseButton(1, true)
		if orientationFirst {
			c.session.setOrientation(3)
		} else {
			c.SetInputDimensions(2896, 1328)
		}
		if len(wire.reports) != 1 || c.session.input.rotation != 0 {
			t.Fatal("changed mapping before video and direction matched")
		}
		if orientationFirst {
			c.SetInputDimensions(2896, 1328)
		} else {
			c.session.setOrientation(3)
		}
		if len(wire.reports) != 2 || c.session.input.contact {
			t.Fatal("rotation left a drag held")
		}
		assertTouchPosition(t, wire.reports[1], false, 8192, 24575)
		_ = c.MouseButton(1, true)
		assertTouchPosition(t, wire.reports[2], true, 8192, 8192)
		c.session.setOrientation(4)
		assertTouchPosition(t, wire.reports[3], false, 8192, 8192)
		_ = c.MouseButton(1, true)
		assertTouchPosition(t, wire.reports[4], true, 24575, 24575)
		c.session.setOrientation(1)
		c.SetInputDimensions(1328, 2896)
		assertTouchPosition(t, wire.reports[5], false, 24575, 24575)
		_ = c.MouseButton(1, true)
		assertTouchPosition(t, wire.reports[6], true, 8192, 24575)
	}
}

func TestDirectTouchDoesNotApplyInterfaceRotationTwice(t *testing.T) {
	c, wire := rotationInputFixture(t, false)
	c.SetInputDimensions(1328, 2896)
	c.session.setOrientation(3)
	_ = c.MouseMove(8192, 24575)
	_ = c.MouseButton(1, true)
	assertTouchPosition(t, wire.reports[0], true, 8192, 24575)
}

func TestDisplayServiceGameStreamTouchKeepsEncodedAxes(t *testing.T) {
	c, wire := rotationInputFixture(t, false)
	c.SetVideoStreamOptions(&capture.VideoStreamOptions{Codec: "hevc", Width: 1280, Height: 720, FPS: 30})
	c.SetInputDimensions(1328, 2896)
	_ = c.MouseMove(8192, 24575)
	_ = c.MouseButton(1, true)
	for _, orientation := range []int32{1, 3, 4, 2} {
		before := len(wire.reports)
		c.session.setOrientation(orientation)
		if len(wire.reports) != before || !c.session.input.contact || c.session.input.rotation != 0 {
			t.Fatal("orientation changed raw GameStream mapping or released its drag")
		}
		if err := c.MouseMove(8192, 24575); err != nil {
			t.Fatal(err)
		}
		assertTouchPosition(t, wire.reports[len(wire.reports)-1], true, 8192, 24575)
	}
	_ = c.MouseMoveRel(1328, 0)
	assertTouchPosition(t, wire.reports[len(wire.reports)-1], true, 32767, 24575)
	_ = c.MouseButton(1, false)
	_ = c.MouseMove(16384, 16384)
	wire.reports = nil
	if err := c.MouseWheel(1); err != nil {
		t.Fatal(err)
	}
	assertTouchPosition(t, wire.reports[0], true, 16384, 16384)
	assertTouchPosition(t, wire.reports[6], true, 16384, 17884)
	assertTouchPosition(t, wire.reports[7], false, 16384, 17884)
}

func TestStandaloneRelativeMouseAndWheelFollowVisibleAxes(t *testing.T) {
	c, wire := rotationInputFixture(t, true)
	c.SetInputDimensions(2896, 1328)
	c.session.setOrientation(3)
	_ = c.MouseMove(16384, 16384)
	_ = c.MouseButton(1, true)
	_ = c.MouseMoveRel(2896, 0)
	assertTouchPosition(t, wire.reports[1], true, 16383, 32767)
	_ = c.MouseButton(1, false)
	_ = c.MouseMove(16384, 16384)
	wire.reports = nil
	if err := c.MouseWheel(1); err != nil {
		t.Fatal(err)
	}
	if len(wire.reports) != 8 {
		t.Fatalf("wheel reports=%d", len(wire.reports))
	}
	assertTouchPosition(t, wire.reports[0], true, 16383, 16384)
	assertTouchPosition(t, wire.reports[6], true, 14883, 16384)
	assertTouchPosition(t, wire.reports[7], false, 14883, 16384)
	if c.session.input.x != 16384 || c.session.input.y != 16384 {
		t.Fatal("wheel changed hover position")
	}
}
