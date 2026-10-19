package hid

import (
	"io"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"
)

func readCH9329Command(r io.Reader) ([]byte, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	frame := append(header, make([]byte, int(header[4])+1)...)
	_, err := io.ReadFull(r, frame[5:])
	return frame, err
}

func testCH9329(t *testing.T, absolute bool) (*ch9329Base, *[]byte) {
	t.Helper()
	client, chip := net.Pipe()
	b := newCH9329Base(client, absolute)
	var reports []byte
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			frame, err := readCH9329Command(chip)
			if err != nil {
				return
			}
			reports = append(reports, frame...)
			if _, err := chip.Write(ch9329Frame(frame[3]|0x80, 0)); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { b.Close(); chip.Close(); <-finished })
	return b, &reports
}

func TestCH9329Drag(t *testing.T) {
	for _, absolute := range []bool{true, false} {
		t.Run(map[bool]string{true: "absolute", false: "relative"}[absolute], func(t *testing.T) {
			b, reports := testCH9329(t, absolute)
			var d hidi
			if absolute {
				d = &ch9329AbsDevice{ch9329Base: b}
			} else {
				d = &ch9329RelDevice{ch9329Base: b}
			}
			for _, action := range []func() error{
				func() error { return d.MouseMove(1000, 2000) },
				func() error { return d.MouseButton(1, true) },
				func() error { return d.MouseMove(1300, 1500) },
				func() error { return d.MouseButton(2, true) },
				func() error { return d.MouseWheel(1) },
				func() error { return d.MouseButton(2, false) },
				func() error { return d.MouseMoveRel(300, -500) },
				func() error { return d.MouseButton(1, false) },
				func() error { return d.MouseMoveRel(1, 1) },
			} {
				if err := action(); err != nil {
					t.Fatal(err)
				}
			}
			b.Close()
			<-b.done
			// The emulator recorded each report before acknowledging it.
			pressed, released := false, false
			totalX, totalY := 0, 0
			for data := *reports; len(data) > 0; {
				n := 6 + int(data[4])
				frame := data[:n]
				data = data[n:]
				if ch9329Checksum(frame[:n-1]) != frame[n-1] {
					t.Fatal("invalid report checksum")
				}
				buttons := frame[6]
				if buttons&1 != 0 {
					if released {
						t.Fatal("left button pressed again after release")
					}
					pressed = true
				} else if pressed {
					if len(data) > n {
						t.Fatal("left button released during drag")
					}
					released = true
				}
				if !absolute {
					totalX += int(int8(frame[7]))
					totalY += int(int8(frame[8]))
				}
			}
			if !pressed || !released {
				t.Fatal("missing drag button transition")
			}
			if !absolute && (totalX != 1601 || totalY != 1001) {
				t.Fatalf("drag distance = (%d, %d), want (1601, 1001)", totalX, totalY)
			}
		})
	}
}

func TestCH9329WaitsForAcknowledgement(t *testing.T) {
	client, chip := net.Pipe()
	b := newCH9329Base(client, true)
	t.Cleanup(func() { b.Close(); chip.Close() })
	result := make(chan error, 1)
	go func() { result <- b.MouseButton(1, true) }()
	if _, err := readCH9329Command(chip); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		t.Fatalf("command returned before acknowledgement: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	bad := ch9329Frame(ch9329AbsoluteCmd|0x80, 0)
	bad[len(bad)-1]++
	for _, part := range [][]byte{{0xff}, bad, ch9329Frame(ch9329RelativeCmd|0x80, 0), {0x57}, ch9329Frame(ch9329AbsoluteCmd|0x80, 0)[1:]} {
		if _, err := chip.Write(part); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestCH9329ConcurrentInput(t *testing.T) {
	b, _ := testCH9329(t, true)
	d := &ch9329AbsDevice{ch9329Base: b}
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 10; n++ {
				for _, err := range []error{d.MouseMove(n, n), d.MouseButton(1, true), d.MouseMoveRel(1, 1), d.MouseWheel(1), d.MouseButton(1, false)} {
					if err != nil {
						t.Error(err)
					}
				}
			}
		}()
	}
	wg.Wait()
}

func TestCH9329CommandFailure(t *testing.T) {
	for _, mode := range []string{"status", "error reply", "timeout", "disconnect"} {
		t.Run(mode, func(t *testing.T) {
			client, chip := net.Pipe()
			b := newCH9329Base(client, true)
			t.Cleanup(func() { b.Close(); chip.Close() })
			result := make(chan error, 1)
			go func() { result <- b.MouseButton(1, true) }()
			if _, err := readCH9329Command(chip); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "status":
				if _, err := chip.Write(ch9329Frame(ch9329AbsoluteCmd|0x80, 0xe6)); err != nil {
					t.Fatal(err)
				}
			case "error reply":
				if _, err := chip.Write(ch9329Frame(ch9329AbsoluteCmd|0xc0, 0xe4)); err != nil {
					t.Fatal(err)
				}
			case "disconnect":
				chip.Close()
			}
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("failed command reported success")
				}
			case <-time.After(time.Second):
				t.Fatal("failed command did not return")
			}
		})
	}
}

func TestCH9329Frame(t *testing.T) {
	tests := []struct {
		name string
		got  []byte
		want []byte
	}{
		{
			name: "keyboard",
			got:  ch9329Frame(ch9329KeyboardCmd, 0, 0, 0x04, 0, 0, 0, 0, 0),
			want: []byte{0x57, 0xAB, 0x00, 0x02, 0x08, 0x00, 0x00, 0x04, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10},
		},
		{
			name: "absolute mouse",
			got:  ch9329Frame(ch9329AbsoluteCmd, 0x02, 0x01, 0xFF, 0x0F, 0x00, 0x00, 0x00),
			want: []byte{0x57, 0xAB, 0x00, 0x04, 0x07, 0x02, 0x01, 0xFF, 0x0F, 0x00, 0x00, 0x00, 0x1E},
		},
		{
			name: "relative mouse",
			got:  ch9329Frame(ch9329RelativeCmd, 0x01, 0x03, 0xFE, 0x02, 0xFF),
			want: []byte{0x57, 0xAB, 0x00, 0x05, 0x05, 0x01, 0x03, 0xFE, 0x02, 0xFF, 0x0F},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !reflect.DeepEqual(tt.got, tt.want) {
				t.Errorf("ch9329Frame() = % X, want % X", tt.got, tt.want)
			}
		})
	}
}

func TestCH9329Coordinate(t *testing.T) {
	tests := []struct {
		input int
		want  int
	}{
		{-1, 0},
		{0, 0},
		{inputAbsoluteMax / 2, 2047},
		{inputAbsoluteMax, ch9329AbsoluteMax},
		{inputAbsoluteMax + 1, ch9329AbsoluteMax},
	}
	for _, tt := range tests {
		if got := ch9329Coordinate(tt.input); got != tt.want {
			t.Errorf("ch9329Coordinate(%d) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestIsCH9329InfoReply(t *testing.T) {
	valid := ch9329Frame(ch9329InfoCmd|0x80, 0x29, 0x01)
	if !isCH9329InfoReply(append([]byte{0x00, 0xFF}, valid...)) {
		t.Fatal("valid CH9329 GET_INFO reply was not recognized")
	}
	invalid := append([]byte(nil), valid...)
	invalid[len(invalid)-1]++
	if isCH9329InfoReply(invalid) {
		t.Fatal("reply with an invalid checksum was recognized")
	}
}
