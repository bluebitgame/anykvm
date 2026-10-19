package discovery

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestDeviceCacheProbesOnceAndReturnsIndependentSnapshots(t *testing.T) {
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	c := newDeviceCache(func() map[string][]DeviceOption {
		calls.Add(1)
		close(entered)
		<-release
		return map[string][]DeviceOption{"hid": {{Value: "hid:test", Label: "HID"}}, "audio": {}}
	})
	c.start()
	<-entered
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := c.get()
			if got["hid"][0].Value != "hid:test" {
				t.Error("missing discovered device")
			}
			got["hid"][0].Value = "changed"
			delete(got, "audio")
		}()
	}
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("probe count = %d", calls.Load())
	}
	got := c.get()
	if got["hid"][0].Value != "hid:test" || got["audio"] == nil {
		t.Fatalf("cache mutated: %#v", got)
	}
}
