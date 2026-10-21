//go:build darwin && cgo

package videoenc

/*
#include "native.h"
*/
import "C"

func platformSupportsCodec(codec string) bool {
	return codec == "hevc" && C.bk_encoder_supports_hevc() != 0
}
