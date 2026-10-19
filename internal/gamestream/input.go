package gamestream

import (
	"encoding/binary"
	"errors"
	"sync"

	"anykvm/internal/bridge"
)

type inputState struct {
	mu                                 sync.Mutex
	hid                                bridge.HIDSender
	active                             bool
	keys                               [256]bool
	buttons                            [4]bool
	wheel                              int
	geometryMu                         sync.Mutex
	sourceW, sourceH, outputW, outputH int
	nativeEncoded                      bool
}

func (i *inputState) release() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.active = false
	if i.hid != nil {
		for k, held := range i.keys {
			if held {
				_ = i.hid.KeyRelease(uint8(k))
			}
		}
		for b, held := range i.buttons {
			if held {
				_ = i.hid.MouseButton(b, false)
			}
		}
	}
	i.keys = [256]bool{}
	i.buttons = [4]bool{}
}
func (i *inputState) handle(p []byte) error {
	if len(p) < 8 || int(binary.BigEndian.Uint32(p)) != len(p)-4 {
		return errors.New("invalid input length")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.active || i.hid == nil {
		return nil
	}
	kind := binary.LittleEndian.Uint32(p[4:])
	s16 := func(n int) int { return int(int16(binary.BigEndian.Uint16(p[n:]))) }
	switch kind {
	case 3, 4:
		if len(p) != 14 {
			return errors.New("invalid keyboard packet")
		}
		vk := binary.LittleEndian.Uint16(p[9:]) & 0xff
		key := virtualKey(vk)
		if vk == 0x0d && p[11]&0x10 != 0 {
			key = 0x58
		}
		if key == 0 {
			return nil
		}
		down := kind == 3
		if i.keys[key] == down {
			return nil
		}
		i.keys[key] = down
		if down {
			return i.hid.KeyDown(key)
		}
		return i.hid.KeyRelease(key)
	case 6, 7:
		if len(p) != 12 {
			return errors.New("invalid relative mouse packet")
		}
		return i.hid.MouseMoveRel(s16(8), s16(10))
	case 5:
		if len(p) != 18 {
			return errors.New("invalid absolute mouse packet")
		}
		w, h := s16(14), s16(16)
		if w <= 0 || h <= 0 {
			return errors.New("invalid pointer dimensions")
		}
		x, y := float64(s16(8))/float64(w), float64(s16(10))/float64(h)
		// Remove encoder letterboxing before converting to the HID 0..32767 range.
		i.geometryMu.Lock()
		if i.nativeEncoded && i.outputW > 0 && i.outputH > 0 {
			// Moonlight's pointer region uses the requested stream aspect,
			// while native passthrough can display a different encoded aspect.
			x, y = removePointerPadding(x, y, float64(i.outputW)/float64(i.outputH), float64(w)/float64(h))
		}
		if i.sourceW > 0 && i.sourceH > 0 && i.outputW > 0 && i.outputH > 0 {
			src := float64(i.sourceW) / float64(i.sourceH)
			out := float64(i.outputW) / float64(i.outputH)
			x, y = removePointerPadding(x, y, src, out)
		}
		i.geometryMu.Unlock()
		return i.hid.MouseMove(int(max(0, min(1, x))*32767), int(max(0, min(1, y))*32767))
	case 8, 9:
		if len(p) != 9 {
			return errors.New("invalid mouse button packet")
		}
		// GameStream: left=1, middle=2, right=3. AnyKVM: left=1, right=2, middle=3.
		b := int(p[8])
		if b < 1 || b > 3 {
			return nil
		}
		b = []int{0, 1, 3, 2}[b]
		down := kind == 8
		if i.buttons[b] == down {
			return nil
		}
		i.buttons[b] = down
		return i.hid.MouseButton(b, down)
	case 10:
		if len(p) != 14 {
			return errors.New("invalid scroll packet")
		}
		i.wheel += s16(8)
		steps := i.wheel / 120
		i.wheel %= 120
		if steps != 0 {
			return i.hid.MouseWheel(steps)
		}
	}
	return nil
}

func removePointerPadding(x, y, sourceAspect, canvasAspect float64) (float64, float64) {
	if sourceAspect > canvasAspect {
		fraction := canvasAspect / sourceAspect
		y = (y - (1-fraction)/2) / fraction
	} else {
		fraction := sourceAspect / canvasAspect
		x = (x - (1-fraction)/2) / fraction
	}
	return x, y
}
func virtualKey(v uint16) uint8 {
	if v >= 'A' && v <= 'Z' {
		return uint8(v - 'A' + 4)
	}
	if v >= '1' && v <= '9' {
		return uint8(v - '1' + 0x1e)
	}
	if v >= 0x70 && v <= 0x7b {
		return uint8(v - 0x70 + 0x3a)
	}
	if v >= 0x7c && v <= 0x87 {
		return uint8(v - 0x7c + 0x68)
	}
	if v >= 0x61 && v <= 0x69 {
		return uint8(v - 0x61 + 0x59)
	}
	return map[uint16]uint8{0x30: 0x27, 0x08: 0x2a, 0x09: 0x2b, 0x0d: 0x28, 0x1b: 0x29, 0x20: 0x2c, 0x14: 0x39, 0x21: 0x4b, 0x22: 0x4e, 0x23: 0x4d, 0x24: 0x4a, 0x25: 0x50, 0x26: 0x52, 0x27: 0x4f, 0x28: 0x51, 0x2c: 0x46, 0x2d: 0x49, 0x2e: 0x4c, 0x13: 0x48, 0x90: 0x53, 0x91: 0x47, 0x60: 0x62, 0x6a: 0x55, 0x6b: 0x57, 0x6d: 0x56, 0x6e: 0x63, 0x6f: 0x54, 0x10: 0xe1, 0x11: 0xe0, 0x12: 0xe2, 0xa0: 0xe1, 0xa1: 0xe5, 0xa2: 0xe0, 0xa3: 0xe4, 0xa4: 0xe2, 0xa5: 0xe6, 0x5b: 0xe3, 0x5c: 0xe7, 0x5d: 0x65, 0xba: 0x33, 0xbb: 0x2e, 0xbc: 0x36, 0xbd: 0x2d, 0xbe: 0x37, 0xbf: 0x38, 0xc0: 0x35, 0xdb: 0x2f, 0xdc: 0x31, 0xdd: 0x30, 0xde: 0x34, 0xe2: 0x64}[v]
}
