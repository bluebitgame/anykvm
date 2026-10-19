package airplay

import "anykvm/internal/videodec"

type h264Info = videodec.H264Info

func parseH264SPS(nal []byte) (h264Info, error) { return videodec.ParseSPS(nal) }
