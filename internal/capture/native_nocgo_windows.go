//go:build windows && !cgo

package capture

import "fmt"

func newWindowsDevice(string) Device {
	return Failed(fmt.Errorf("Windows native capture requires CGO_ENABLED=1"))
}
func newWindowsAudioDevice(string) AudioDevice { return &unavailableWindowsAudio{} }

func newWASAPIAudioDevice(string) AudioDevice { return &unavailableWindowsAudio{} }

type unavailableWindowsAudio struct{}

func (*unavailableWindowsAudio) Open() error {
	return fmt.Errorf("Windows native audio capture requires CGO_ENABLED=1")
}
func (*unavailableWindowsAudio) Capture() (*AudioFrame, error) { return nil, ErrStreamStopped }
func (*unavailableWindowsAudio) Close() error                  { return nil }
