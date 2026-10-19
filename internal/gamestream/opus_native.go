package gamestream

// nativeOpusEncoder returns raw Opus packets, without an Ogg container.
type nativeOpusEncoder interface {
	Encode([]byte) ([]byte, error)
	Close()
}
