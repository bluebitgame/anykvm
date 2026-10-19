package airplay

import (
	"bufio"
	"net"
	"testing"
	"time"

	"anykvm/internal/capture"

	"howett.net/plist"
)

func TestReceiverBeforeViewerPreservesFirstFrameAndDiscovery(t *testing.T) {
	d, err := NewDevice("airplay:Startup-Test", Options{})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d.receiver.start(listener, nil) // Use a real control socket without Bonjour.
	t.Cleanup(func() { d.Close(); d.ShutdownReceiver() })
	if err := d.StartReceiver(1920, 1080, 60); err != nil {
		t.Fatal(err)
	}
	if d.open {
		t.Fatal("receiver startup opened viewer capture")
	}
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sender := testSender{conn: conn, reader: bufio.NewReader(conn)}
	if status, _ := sender.exchange(t, "GET", "/info", nil); status != 200 {
		t.Fatal("receiver unavailable before viewer startup")
	}
	// Queue the native IDR before capture attaches. Opening must retain the
	// listener, media ownership, payload and source timestamp.
	owner := &pairing{control: conn}
	first := &capture.Frame{Format: capture.FormatH264, Width: 500, Height: 1080,
		CapturedAt: time.Now(), Data: []byte{0, 0, 0, 1, 0x65, 0xab}}
	d.receiver.mu.Lock()
	d.receiver.owner = owner
	d.receiver.frames <- receivedFrame{frame: first, owner: owner}
	d.receiver.mu.Unlock()
	d.SetVideoCodec("h264")
	if err := d.OpenWithFormat(1920, 1080, 60); err != nil {
		t.Fatal(err)
	}
	got, err := d.Capture()
	if err != nil || got != first {
		t.Fatalf("first IDR lost when viewer attached: %p %v", got, err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if d.receiver.listener != listener || d.receiver.owner != nil {
		t.Fatal("viewer detach unregistered receiver or retained sender ownership")
	}
	// A new sender must still reach the receiver after the viewer quits.
	next, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	newSender := testSender{conn: next, reader: bufio.NewReader(next)}
	if status, _ := newSender.exchange(t, "GET", "/info", nil); status != 200 {
		t.Fatal(status)
	}
	if err := d.ShutdownReceiver(); err != nil {
		t.Fatal(err)
	}
	if d.receiver.listener != nil {
		t.Fatal("process shutdown left receiver running")
	}
}

// Exercise the capture API and the wire response without registering test
// receivers on the user's Bonjour network.
func TestCaptureDisplayNegotiation(t *testing.T) {
	for _, tc := range []struct {
		name         string
		w, h, fps    int
		wantW, wantH uint64
		wantFPS      uint64
	}{
		{"default", 0, 0, 0, 1920, 1080, 30},
		{"native60", 0, 0, 60, 1920, 1080, 60},
		{"720p60", 1280, 720, 60, 1280, 720, 60},
		{"360p30", 640, 360, 30, 640, 360, 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := NewDevice("airplay:Display-Test", Options{})
			if err != nil {
				t.Fatal(err)
			}
			d.SetVideoCodec("h264")
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			d.receiver.start(listener, nil)
			t.Cleanup(func() { d.Close() })
			if err := d.OpenWithFormat(tc.w, tc.h, tc.fps); err != nil {
				t.Fatal(err)
			}
			conn, err := net.Dial("tcp", listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			sender := testSender{conn: conn, reader: bufio.NewReader(conn)}
			status, body := sender.exchange(t, "GET", "/info", nil)
			var info struct {
				Displays []struct {
					Width        uint64  `plist:"width"`
					Height       uint64  `plist:"height"`
					WidthPixels  uint64  `plist:"widthPixels"`
					HeightPixels uint64  `plist:"heightPixels"`
					MaxFPS       uint64  `plist:"maxFPS"`
					RefreshRate  float64 `plist:"refreshRate"`
				} `plist:"displays"`
			}
			if _, err := plist.Unmarshal(body, &info); err != nil || status != 200 || len(info.Displays) != 1 {
				t.Fatalf("info status=%d displays=%v err=%v", status, info.Displays, err)
			}
			display := info.Displays[0]
			if display.Width != tc.wantW || display.Height != tc.wantH || display.WidthPixels != tc.wantW || display.HeightPixels != tc.wantH || display.MaxFPS != tc.wantFPS || display.RefreshRate != 1.0/float64(tc.wantFPS) {
				t.Fatalf("advertised display %+v", display)
			}
		})
	}
}

func TestCaptureRejectsInvalidDisplayBeforeListening(t *testing.T) {
	for _, size := range [][3]int{{-1, 720, 60}, {1280, 0, 60}, {0, 720, 60}, {8193, 720, 60}, {1280, 8193, 60}, {1280, 720, -1}, {1280, 720, 256}} {
		d, err := NewDevice("airplay:Display-Test", Options{})
		if err != nil {
			t.Fatal(err)
		}
		d.SetVideoCodec("h264")
		if err := d.OpenWithFormat(size[0], size[1], size[2]); err == nil {
			t.Fatalf("accepted invalid size %v", size)
		}
		if d.open || d.receiver.listener != nil || d.receiver.width != 1920 || d.receiver.height != 1080 || d.receiver.fps != 30 {
			t.Fatalf("invalid size %v mutated receiver", size)
		}
	}
}
