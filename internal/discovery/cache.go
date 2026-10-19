package discovery

import "sync"

var configDeviceCache = newDeviceCache(func() map[string][]DeviceOption { return discoverDevices(false) })
var iosDeviceCache = newDeviceCache(func() map[string][]DeviceOption { return discoverDevices(true) })

func cachedDevices(includeIOS bool) map[string][]DeviceOption {
	if includeIOS {
		return iosDeviceCache.get()
	}
	return configDeviceCache.get()
}

// StartConfigDeviceDiscovery warms the process-wide device list in the background.
// Subsequent settings requests reuse this startup snapshot without probing ports.
func StartConfigDeviceDiscovery() { configDeviceCache.start() }

type deviceCache struct {
	once    sync.Once
	done    chan struct{}
	load    func() map[string][]DeviceOption
	devices map[string][]DeviceOption
}

func newDeviceCache(load func() map[string][]DeviceOption) *deviceCache {
	return &deviceCache{done: make(chan struct{}), load: load}
}

func (c *deviceCache) start() {
	c.once.Do(func() {
		go func() {
			defer close(c.done)
			c.devices = c.load()
		}()
	})
}

func (c *deviceCache) get() map[string][]DeviceOption {
	c.start()
	<-c.done
	result := make(map[string][]DeviceOption, len(c.devices))
	for kind, options := range c.devices {
		result[kind] = append([]DeviceOption{}, options...)
	}
	return result
}
