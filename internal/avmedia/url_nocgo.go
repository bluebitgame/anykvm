//go:build !ffmpeg || !cgo

package avmedia

import "context"

type MediaSource struct{}

func URLMediaAvailable() error { return Available() }

func openMedia(context.Context, string) (*MediaSource, error) { return nil, Available() }
func (*MediaSource) Duration() float64                        { return 0 }
func (*MediaSource) Seek(float64) error                       { return Available() }
func (*MediaSource) Read() (*MediaPacket, error)              { return nil, Available() }
func (*MediaSource) Close()                                   {}
