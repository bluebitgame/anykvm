package gamestream

import (
	"testing"
	"time"
)

func TestQPCVideoClockIgnoresHostTimeAndPreserves90kCadence(t *testing.T) {
	var c videoPlaybackClock
	for i, pts := range []int64{123456789, 123473456, 123490123} {
		diagnostic := time.Now().Add(time.Duration(i-1) * time.Hour)
		stamp, err := c.timestamp(pts, diagnostic, diagnostic.Add(time.Hour), time.Now(), 60, false)
		if err != nil || stamp != 90+uint32(i*1500) {
			t.Fatalf("QPC clock changed with Go time: %d, %v", stamp, err)
		}
	}
}

func TestQPCVideoClockPlaceholderRepeatAndSourceSwitch(t *testing.T) {
	var c videoPlaybackClock
	epoch := time.Now()
	stamp := func(pts int64, repeat bool, elapsed time.Duration) uint32 {
		t.Helper()
		v, err := c.timestamp(pts, time.Time{}, epoch, epoch.Add(elapsed), 60, repeat)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if got := stamp(0, false, time.Second); got != 90000 {
		t.Fatal(got)
	}
	first := stamp(123456789, false, 2*time.Second)
	if first != 91500 {
		t.Fatal("placeholder transition moved backwards", first)
	}
	if got := stamp(123473456, false, 8*time.Second); got != first+1500 {
		t.Fatal(got)
	}
	if got := stamp(123473456, true, 9*time.Second); got != first+3000 {
		t.Fatal(got)
	}
	// A clock/source reset is bridged once. Subsequent QPC intervals survive.
	reset := stamp(1000000, false, 20*time.Second)
	if reset != first+4500 {
		t.Fatal(reset)
	}
	if got := stamp(1016667, false, 21*time.Second); got != reset+1500 {
		t.Fatal(got)
	}
	legacy := stamp(0, false, 25*time.Second)
	if legacy != reset+3000 {
		t.Fatal("legacy source transition moved backwards", legacy)
	}
}

func TestQPCVideoClockWrapAndInvalidPTS(t *testing.T) {
	c := videoPlaybackClock{initialized: true, sourceActive: true, sourceOrigin: 1000000, lastPTS: 1000000, sourceBase: 0xffffff00, last: 0xffffff00}
	stamp, err := c.timestamp(1016667, time.Time{}, time.Time{}, time.Time{}, 60, false)
	if err != nil || stamp != 1244 {
		t.Fatalf("RTP wrap: %d %v", stamp, err)
	}
	for _, pts := range []int64{-1, 1 << 53} {
		if _, err := c.timestamp(pts, time.Now(), time.Now(), time.Now(), 60, false); err == nil {
			t.Fatal("invalid source PTS accepted")
		}
	}
	p := sourceFramePacer{fps: 60}
	now := time.Now()
	if !p.reservePlayback(1000000, now.Add(time.Hour), now).Equal(now) {
		t.Fatal("first pacing slot moved")
	}
	if !p.reservePlayback(1016667, now.Add(-time.Hour), now.Add(time.Millisecond)).Equal(now.Add(16667 * time.Microsecond)) {
		t.Fatal("QPC pacing used diagnostic time")
	}
}
