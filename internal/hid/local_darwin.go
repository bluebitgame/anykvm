//go:build darwin

package hid

/*
#cgo LDFLAGS: -framework CoreGraphics -framework CoreFoundation
#include <CoreGraphics/CoreGraphics.h>
#include <stdlib.h>
#include <time.h>

typedef struct {
 CGEventSourceRef mouseSource, keyboardSource;
 CGEventRef mouseEvent;
} anykvm_input;

static void anykvm_cleanup(anykvm_input *input) {
 if (!input) return;
 if (input->mouseEvent) CFRelease(input->mouseEvent);
 if (input->mouseSource) CFRelease(input->mouseSource);
 if (input->keyboardSource) CFRelease(input->keyboardSource);
 free(input);
}
static anykvm_input *anykvm_init(void) {
 anykvm_input *input = calloc(1, sizeof(*input));
 if (!input) return NULL;
 input->mouseSource = CGEventSourceCreate(kCGEventSourceStateHIDSystemState);
 input->keyboardSource = CGEventSourceCreate(kCGEventSourceStatePrivate);
 if (input->mouseSource) {
  CGEventSourceSetLocalEventsSuppressionInterval(input->mouseSource, 0);
  input->mouseEvent = CGEventCreate(input->mouseSource);
 }
 if (!input->mouseEvent || !input->keyboardSource) { anykvm_cleanup(input); return NULL; }
 return input;
}
static void anykvm_mouse_location(anykvm_input *input, int *x, int *y) {
 // Read from the same HID state table used to post motion (as Sunshine does).
 CGEventRef event = CGEventCreate(input->mouseSource);
 if (!event) return;
 CGPoint point = CGEventGetLocation(event);
 *x = (int)point.x; *y = (int)point.y;
 CFRelease(event);
}

// CGEvent positions use logical desktop coordinates, including display origin.
static void anykvm_screen_bounds(unsigned int display, int *x, int *y, int *w, int *h) {
    CGDirectDisplayID d = display ? display : CGMainDisplayID();
    CGRect bounds = CGDisplayBounds(d);
    *x = (int)bounds.origin.x;
    *y = (int)bounds.origin.y;
    *w = (int)bounds.size.width;
    *h = (int)bounds.size.height;
}

// anykvm_key posts a keyboard event for the given virtual keycode with the
// given modifier flags. down != 0 for key press.
static void anykvm_key(anykvm_input *input, unsigned short vk, int down, unsigned long long flags) {
    CGEventRef e = CGEventCreateKeyboardEvent(input->keyboardSource, (CGKeyCode)vk, down != 0);
    if (e) {
        CGEventSetFlags(e, (CGEventFlags)flags);
        CGEventPost(kCGHIDEventTap, e);
        CFRelease(e);
    }
}

static int anykvm_text(anykvm_input *input, const UniChar *text, size_t length) {
    CGEventRef down = CGEventCreateKeyboardEvent(input->keyboardSource, 0, true);
    CGEventRef up = CGEventCreateKeyboardEvent(input->keyboardSource, 0, false);
    if (!down || !up) {
        if (down) CFRelease(down);
        if (up) CFRelease(up);
        return 0;
    }
    CGEventSetFlags(down, 0);
    CGEventSetFlags(up, 0);
    CGEventKeyboardSetUnicodeString(down, length, text);
    CGEventKeyboardSetUnicodeString(up, length, text);
    // Use the HID entry point for both physical keys and committed text.
    // In particular, IME replacement Backspaces and Unicode must enter the
    // same event stream, in order, before the session's keyboard processing.
    CGEventPost(kCGHIDEventTap, down);
    CGEventPost(kCGHIDEventTap, up);
    CFRelease(down);
    CFRelease(up);
    return 1;
}

// anykvm_mouse posts a mouse event at absolute pixel (x,y). type selects the
// event; button: 0=left,1=right,2=other. flags carries keyboard modifiers.
// Matches Sunshine's CoreGraphics backend: include deltas, post at HID level,
// and move the visible cursor explicitly. A reused event avoids per-move allocation.
static void anykvm_configure_mouse(anykvm_input *input, int x, int y, int dx, int dy, int etype, int button, unsigned long long flags, int clicks) {
 CGEventRef e = input->mouseEvent;
 CGPoint point = CGPointMake(x,y);
 CGEventSetType(e, (CGEventType)etype);
 CGEventSetLocation(e, point);
 CGEventSetIntegerValueField(e, kCGMouseEventButtonNumber, button);
 CGEventSetIntegerValueField(e, kCGMouseEventClickState, clicks);
 CGEventSetIntegerValueField(e, kCGMouseEventDeltaX, dx);
 CGEventSetIntegerValueField(e, kCGMouseEventDeltaY, dy);
 CGEventSetFlags(e, (CGEventFlags)flags);
 struct timespec now;
 clock_gettime(CLOCK_UPTIME_RAW, &now);
 CGEventSetTimestamp(e, (uint64_t)now.tv_sec * 1000000000ULL + now.tv_nsec);
}
static void anykvm_mouse(anykvm_input *input, int x, int y, int dx, int dy, int etype, int button, unsigned long long flags, int clicks) {
 anykvm_configure_mouse(input,x,y,dx,dy,etype,button,flags,clicks);
 CGEventPost(kCGHIDEventTap, input->mouseEvent);
 CGWarpMouseCursorPosition(CGPointMake(x,y));
}

// anykvm_scroll posts a vertical scroll-wheel event.
static void anykvm_scroll(anykvm_input *input, int dy) {
    CGEventRef e = CGEventCreateScrollWheelEvent(input->mouseSource, kCGScrollEventUnitLine, 1, (int32_t)dy);
    if (e) {
        CGEventPost(kCGSessionEventTap, e);
        CFRelease(e);
    }
}
*/
import "C"

import (
	"fmt"
	"sync"
	"time"
	"unicode/utf16"
)

// CGEventType values (from CoreGraphics CGEventTypes.h).
const (
	cgEventMouseMoved     = 5
	cgEventLeftMouseDown  = 1
	cgEventLeftMouseUp    = 2
	cgEventRightMouseDown = 3
	cgEventRightMouseUp   = 4
	cgEventOtherMouseDown = 25
	cgEventOtherMouseUp   = 26
	cgEventLeftMouseDrag  = 6
	cgEventRightMouseDrag = 7
	cgEventOtherMouseDrag = 27
)

// localDarwinHID injects keyboard/mouse events into the local macOS session via
// CoreGraphics CGEvent. It implements the hidi interface, so the VNC/RDP
// servers can drive the local machine as if it were a remote target.
//
// Requires the "Accessibility" permission (System Settings -> Privacy &
// Security -> Accessibility) for CGEventPost to take effect.
type localDarwinHID struct {
	mu               sync.Mutex
	native           *C.anykvm_input
	screenW          int
	screenH          int
	displayID        uint32
	screenX, screenY int
	mouseX           int // last absolute pixel position
	mouseY           int
	buttonsDown      uint8 // bitmask of held buttons (bit0 left, bit1 right, bit2 middle)
	flags            uint64
	pressed          map[uint8]bool // held HID keycodes (for flag maintenance)

	// click counting for double-click support
	lastClickTime     time.Time
	lastClickX        int
	lastClickY        int
	consecutiveClicks int
}

func openLocalDarwin() (hidi, error) {
	var originX, originY, w, h C.int
	C.anykvm_screen_bounds(0, &originX, &originY, &w, &h)
	native := C.anykvm_init()
	if native == nil {
		return nil, fmt.Errorf("macOS: could not create input event sources")
	}
	var x, y C.int
	C.anykvm_mouse_location(native, &x, &y)
	return &localDarwinHID{
		native: native,
		mouseX: int(x), mouseY: int(y),
		screenW: int(w),
		screenH: int(h),
		screenX: int(originX), screenY: int(originY),
		pressed: make(map[uint8]bool),
	}, nil
}

// openLocal is the platform entry point used by OpenLocal.
func openLocal() (hidi, error) {
	return openLocalDarwin()
}

func (d *localDarwinHID) KeyDown(code uint8) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.native == nil {
		return fmt.Errorf("macOS input closed")
	}
	d.pressed[code] = true
	d.flags |= hidModifierMask(code)
	vk, ok := hidToCGKeyCode[code]
	if !ok {
		return nil // unmapped key: ignore
	}
	C.anykvm_key(d.native, C.ushort(vk), 1, C.ulonglong(d.flags))
	return nil
}

func (d *localDarwinHID) KeyRelease(code uint8) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.native == nil {
		return fmt.Errorf("macOS input closed")
	}
	delete(d.pressed, code)
	d.flags &^= hidModifierMask(code)
	vk, ok := hidToCGKeyCode[code]
	if !ok {
		return nil
	}
	C.anykvm_key(d.native, C.ushort(vk), 0, C.ulonglong(d.flags))
	return nil
}

func (d *localDarwinHID) SendText(text string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.native == nil {
		return fmt.Errorf("macOS input closed")
	}
	for _, r := range text {
		units := utf16.Encode([]rune{r})
		if C.anykvm_text(d.native, (*C.UniChar)(&units[0]), C.size_t(len(units))) == 0 {
			return fmt.Errorf("macOS: could not create Unicode keyboard events")
		}
	}
	return nil
}

// KeyUp releases all currently-held keys.
func (d *localDarwinHID) KeyUp() error {
	d.mu.Lock()
	held := make([]uint8, 0, len(d.pressed))
	for k := range d.pressed {
		held = append(held, k)
	}
	d.mu.Unlock()
	for _, k := range held {
		d.KeyRelease(k)
	}
	return nil
}

// MouseMove receives absolute coordinates in the 0..32767 range (as sent by the
// VNC/RDP servers) and maps them to the selected display's logical coordinates.
func (d *localDarwinHID) MouseMove(x, y int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.native == nil {
		return fmt.Errorf("macOS input closed")
	}
	oldX, oldY := d.currentMousePosition()
	d.refreshDisplayBounds()
	d.mouseX = normalizedDisplayPoint(x, d.screenX, d.screenW)
	d.mouseY = normalizedDisplayPoint(y, d.screenY, d.screenH)
	etype, btn := cgEventMouseMoved, 0
	switch {
	case d.buttonsDown&0x01 != 0:
		etype, btn = cgEventLeftMouseDrag, 0
	case d.buttonsDown&0x02 != 0:
		etype, btn = cgEventRightMouseDrag, 1
	case d.buttonsDown&0x04 != 0:
		etype, btn = cgEventOtherMouseDrag, 2
	}
	flags := d.flags
	C.anykvm_mouse(d.native, C.int(d.mouseX), C.int(d.mouseY), C.int(d.mouseX-oldX), C.int(d.mouseY-oldY), C.int(etype), C.int(btn), C.ulonglong(flags), 0)
	return nil
}

// MouseMoveRel applies a relative pixel delta to the current cursor position
// and posts a CGEvent at the new location. Used by the VNC QEMU -263
// relative-mouse path.
func (d *localDarwinHID) SetDisplayID(id uint32) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.displayID = id
	d.refreshDisplayBounds()
}

// Caller holds mu. Refresh on motion so restoration and external display-mode
// changes cannot leave normalized input using obsolete dimensions.
func (d *localDarwinHID) refreshDisplayBounds() {
	var x, y, w, h C.int
	C.anykvm_screen_bounds(C.uint(d.displayID), &x, &y, &w, &h)
	if w > 0 && h > 0 {
		d.screenX, d.screenY, d.screenW, d.screenH = int(x), int(y), int(w), int(h)
	}
}

func (d *localDarwinHID) MouseMoveRel(dx, dy int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.native == nil {
		return fmt.Errorf("macOS input closed")
	}
	oldX, oldY := d.currentMousePosition()
	d.refreshDisplayBounds()
	d.mouseX = clamp(oldX+dx, d.screenX, d.screenX+d.screenW-1)
	d.mouseY = clamp(oldY+dy, d.screenY, d.screenY+d.screenH-1)
	etype, btn := cgEventMouseMoved, 0
	switch {
	case d.buttonsDown&0x01 != 0:
		etype, btn = cgEventLeftMouseDrag, 0
	case d.buttonsDown&0x02 != 0:
		etype, btn = cgEventRightMouseDrag, 1
	case d.buttonsDown&0x04 != 0:
		etype, btn = cgEventOtherMouseDrag, 2
	}
	flags := d.flags
	C.anykvm_mouse(d.native, C.int(d.mouseX), C.int(d.mouseY), C.int(dx), C.int(dy), C.int(etype), C.int(btn), C.ulonglong(flags), 0)
	return nil
}

var mouseBtnConfig = []struct {
	mask                    uint8
	cgBtn, downType, upType int
}{
	{},
	{0x01, 0, cgEventLeftMouseDown, cgEventLeftMouseUp},
	{0x02, 1, cgEventRightMouseDown, cgEventRightMouseUp},
	{0x04, 2, cgEventOtherMouseDown, cgEventOtherMouseUp},
}

func (d *localDarwinHID) MouseButton(button int, down bool) error {
	if button < 1 || button > 3 {
		return nil
	}
	cfg := mouseBtnConfig[button]

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.native == nil {
		return fmt.Errorf("macOS input closed")
	}
	d.mouseX, d.mouseY = d.currentMousePosition()
	etype := cfg.upType
	if down {
		etype = cfg.downType
		d.buttonsDown |= cfg.mask
		if button == 1 {
			now := time.Now()
			dx, dy := d.mouseX-d.lastClickX, d.mouseY-d.lastClickY
			if now.Sub(d.lastClickTime) < 500*time.Millisecond && dx >= -4 && dx <= 4 && dy >= -4 && dy <= 4 {
				d.consecutiveClicks++
			} else {
				d.consecutiveClicks = 1
			}
			d.lastClickTime, d.lastClickX, d.lastClickY = now, d.mouseX, d.mouseY
		} else {
			d.consecutiveClicks = 1
		}
	} else {
		d.buttonsDown &^= cfg.mask
	}
	clicks := d.consecutiveClicks
	if button != 1 {
		clicks = 1
	}
	px, py, flags := d.mouseX, d.mouseY, d.flags
	C.anykvm_mouse(d.native, C.int(px), C.int(py), 0, 0, C.int(etype), C.int(cfg.cgBtn), C.ulonglong(flags), C.int(clicks))
	return nil
}

func (d *localDarwinHID) MouseWheel(delta int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.native == nil {
		return fmt.Errorf("macOS input closed")
	}
	C.anykvm_scroll(d.native, C.int(delta))
	return nil
}

func (d *localDarwinHID) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	C.anykvm_cleanup(d.native)
	d.native = nil
	return nil
}

// Caller holds mu. Reading the OS position avoids jumping back to a stale
// software position after local input or application cursor warping.
func (d *localDarwinHID) currentMousePosition() (int, int) {
	x, y := C.int(d.mouseX), C.int(d.mouseY)
	C.anykvm_mouse_location(d.native, &x, &y)
	return int(x), int(y)
}
