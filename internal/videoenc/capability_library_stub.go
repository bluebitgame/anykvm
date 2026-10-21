//go:build linux && cgo && !ffmpeg

package videoenc

func librarySupportsCodec(string) bool { return false }
