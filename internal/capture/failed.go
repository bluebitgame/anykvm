package capture

// Failed returns a source that reports the original setup error when opened.
// It prevents invalid remote sources from falling back to local discovery.
func Failed(err error) Device { return &failedDevice{err: err} }

// Invalid remote configuration must never turn into local device discovery.
type failedDevice struct{ err error }

func (d *failedDevice) Open() error                        { return d.err }
func (d *failedDevice) OpenWithFormat(int, int, int) error { return d.err }
func (d *failedDevice) Capture() (*Frame, error)           { return nil, d.err }
func (*failedDevice) Close() error                         { return nil }
func (*failedDevice) SetJPEGQuality(int)                   {}
func (*failedDevice) SetDecodeImage(bool)                  {}
