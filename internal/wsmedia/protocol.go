package wsmedia

// Frame protocol (WebSocket binary messages): [1B type][payload].
//
// Server -> Client:
//
//	0x00 metadata (JSON: {"w":int,"h":int,"rotation":0|90|180|270,"sampleRate":int,"channels":int,"bufferH264":bool})
//	     Initial metadata includes mouseMode (absolute/relative) and videoBufferFrames (1..8, default 1).
//	     Later metadata may omit it to preserve the connection's queue capacity.
//	     This listener setting supersedes the legacy bufferH264 source hint.
//	     audioAvailable reports successful audio delivery and source shutdown.
//	0x01 JPEG video frame
//	0x02 PCM audio (S16LE, interleaved)
//	0x03 (reserved) Opus audio
//	0x04 target list (JSON: {"machines":[{"name":"..."}],"active":0})
//	0x05 target switching (JSON: {"switching":true})
//	0x06 compressed audio: [u16 BE JSON length][JSON decoder config][raw access unit]
//	     Decoder config includes codec, config (base64 ASC), sampleRate,
//	     channels, timestamp (microseconds), frames, optional gain and epoch.
//	0x10 H264 video: [0x10][u64 LE presentation time, Unix microseconds][Annex-B AU]
//	     Uses the source timestamp, or server arrival time when unavailable.
//
// Client -> Server:
//
//	0x80 keyboard: [0x80][down u8][code ASCII...] where code is a DOM
//	     KeyboardEvent.code (physical key), mapped to a USB HID usage server-side.
//	0x81 pointer:  [0x81][buttons u8][x u16 BE][y u16 BE] in framebuffer pixels;
//	     button bitmask bit0=left,1=middle,2=right,3=wheelUp,4=wheelDown.
//	0x83 hello:    [0x83]{JSON} client capabilities, e.g. {"h264":true}.
//	     audioFormats lists exact supported compressed audio configurations.
//	0x82 (reserved) gamepad state
//	0x84 (reserved) client target switch request
//	0x85 request a fresh video keyframe (no payload)
//	0x87 relative pointer: [0x87][buttons u8][dx i16 BE][dy i16 BE].
//	     Uses the same button and wheel bitmask as 0x81.
//	0x86 optional video diagnostics (bounded JSON, emitted by debug viewer)
const (
	msgMetadata     = 0x00
	msgJPEG         = 0x01
	msgPCM          = 0x02
	msgMachines     = 0x04
	msgSwitching    = 0x05
	msgEncodedAudio = 0x06
	msgH264         = 0x10
	msgHEVC         = 0x11
	msgAV1          = 0x12
	// client->server
	msgInputKeyboard      = 0x80
	msgInputMouse         = 0x81
	msgInputMouseRelative = 0x87
	msgInputGamepad       = 0x82 // reserved
	msgHello              = 0x83
	msgSwitchReq          = 0x84 // reserved
	msgKeyframeReq        = 0x85
	msgVideoDiagnostics   = 0x86
)
