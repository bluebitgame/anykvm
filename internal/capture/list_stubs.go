//go:build (!darwin && !windows && !linux) || (!cgo && !darwin)

package capture

func ListNativeVideoDevices(includeIOS bool) []string { return nil }
func ListNativeAudioDevices() []string                { return nil }
