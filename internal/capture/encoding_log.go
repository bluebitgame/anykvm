package capture

import "log"

// encodingLog belongs to a capture session, never to an individual frame.
// Its caller holds the device's capture lock.
type encodingLog struct {
	codec, encoder string
}

func (l *encodingLog) start(codec, encoder string) {
	if l.codec == codec && l.encoder == encoder {
		return
	}
	log.Printf("capture: video codec=%s encoder=%q", codec, encoder)
	l.codec, l.encoder = codec, encoder
}
