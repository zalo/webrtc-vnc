//go:build darwin && cgo

package main

/*
#cgo CFLAGS:  -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework ApplicationServices -framework Carbon -framework Foundation

#include <ApplicationServices/ApplicationServices.h>
#include <Carbon/Carbon.h>

static CGEventSourceRef cg_source = NULL;

static void cg_init(void) {
    if (cg_source == NULL) {
        cg_source = CGEventSourceCreate(kCGEventSourceStateHIDSystemState);
    }
}

static void cg_mouse_move_abs(double x_norm, double y_norm) {
    cg_init();
    CGRect bounds = CGDisplayBounds(CGMainDisplayID());
    CGFloat x = bounds.origin.x + (CGFloat)x_norm * bounds.size.width;
    CGFloat y = bounds.origin.y + (CGFloat)y_norm * bounds.size.height;
    CGEventRef ev = CGEventCreateMouseEvent(cg_source, kCGEventMouseMoved,
                                            CGPointMake(x, y), kCGMouseButtonLeft);
    CGEventPost(kCGHIDEventTap, ev);
    CFRelease(ev);
}

static void cg_mouse_move_rel(int dx, int dy) {
    cg_init();
    CGEventRef get = CGEventCreate(NULL);
    CGPoint cur = CGEventGetLocation(get);
    CFRelease(get);
    CGPoint dst = CGPointMake(cur.x + dx, cur.y + dy);
    CGEventRef ev = CGEventCreateMouseEvent(cg_source, kCGEventMouseMoved, dst, kCGMouseButtonLeft);
    CGEventPost(kCGHIDEventTap, ev);
    CFRelease(ev);
}

static void cg_mouse_button(int button, int down) {
    cg_init();
    CGEventRef get = CGEventCreate(NULL);
    CGPoint cur = CGEventGetLocation(get);
    CFRelease(get);

    CGEventType evt;
    CGMouseButton btn;
    switch (button) {
        case 0:  btn = kCGMouseButtonLeft;   evt = down ? kCGEventLeftMouseDown   : kCGEventLeftMouseUp;   break;
        case 1:  btn = kCGMouseButtonCenter; evt = down ? kCGEventOtherMouseDown  : kCGEventOtherMouseUp;  break;
        case 2:  btn = kCGMouseButtonRight;  evt = down ? kCGEventRightMouseDown  : kCGEventRightMouseUp;  break;
        default: btn = (CGMouseButton)button; evt = down ? kCGEventOtherMouseDown : kCGEventOtherMouseUp;  break;
    }
    CGEventRef ev = CGEventCreateMouseEvent(cg_source, evt, cur, btn);
    CGEventPost(kCGHIDEventTap, ev);
    CFRelease(ev);
}

static void cg_scroll(int dx, int dy) {
    cg_init();
    CGEventRef ev = CGEventCreateScrollWheelEvent(cg_source, kCGScrollEventUnitLine, 2, dy, dx);
    CGEventPost(kCGHIDEventTap, ev);
    CFRelease(ev);
}

static void cg_key(int keycode, int down) {
    cg_init();
    CGEventRef ev = CGEventCreateKeyboardEvent(cg_source, (CGKeyCode)keycode, down ? true : false);
    CGEventPost(kCGHIDEventTap, ev);
    CFRelease(ev);
}
*/
import "C"

import (
	"encoding/binary"
	"log"
	"sync"
)

// osCapture stub — capture_darwin.go will define the real one.
// (We'll add osCapture in capture_darwin.go shortly.)

// Input on macOS posts CG events. Requires Accessibility permission.
type Input struct {
	mu sync.Mutex
}

func NewInput() (*Input, error) {
	log.Printf("Input: macOS CGEvent backend (requires Accessibility permission)")
	return &Input{}, nil
}

func (inp *Input) Close() {}

func (inp *Input) EnsureGamepad(slot int) {
	// TODO: macOS gamepad emulation requires a virtual HID driver kext or
	// DriverKit dext. Out of scope for now.
	_ = slot
}

// HandleKeyboard: [0x02][keycode:u16le][modifiers:u8][pressed:u8]
func (inp *Input) HandleKeyboard(data []byte) {
	if len(data) < 5 {
		return
	}
	vk := binary.LittleEndian.Uint16(data[1:3])
	pressed := data[4] != 0

	mac := vkToMac(vk)
	if mac < 0 {
		return
	}
	inp.mu.Lock()
	defer inp.mu.Unlock()
	C.cg_key(C.int(mac), boolToCInt(pressed))
}

// HandleMouseMove: [0x03][flags:u8][x:u16le][y:u16le]
//   flags bit0 = absolute (0..65535 → screen rect)
//   else relative delta (signed int16)
func (inp *Input) HandleMouseMove(data []byte) {
	if len(data) < 6 {
		return
	}
	flags := data[1]
	x := int(binary.LittleEndian.Uint16(data[2:4]))
	y := int(binary.LittleEndian.Uint16(data[4:6]))

	inp.mu.Lock()
	defer inp.mu.Unlock()
	if flags&0x01 != 0 {
		C.cg_mouse_move_abs(C.double(float64(x)/65535.0), C.double(float64(y)/65535.0))
	} else {
		C.cg_mouse_move_rel(C.int(int16(x)), C.int(int16(y)))
	}
}

// HandleMouseButton: [0x04][button:u8][pressed:u8]
//   button: 0=left, 1=middle, 2=right
func (inp *Input) HandleMouseButton(data []byte) {
	if len(data) < 3 {
		return
	}
	inp.mu.Lock()
	defer inp.mu.Unlock()
	C.cg_mouse_button(C.int(data[1]), boolToCInt(data[2] != 0))
}

// HandleMouseScroll: [0x05][reserved:u8][dx:i16le][dy:i16le]
func (inp *Input) HandleMouseScroll(data []byte) {
	if len(data) < 6 {
		return
	}
	dx := int16(binary.LittleEndian.Uint16(data[2:4]))
	dy := int16(binary.LittleEndian.Uint16(data[4:6]))
	// Browsers send wheel deltas in pixels-ish. Normalize to ±1 line.
	ndx := int32(0)
	if dx > 0 { ndx = 1 } else if dx < 0 { ndx = -1 }
	ndy := int32(0)
	if dy > 0 { ndy = 1 } else if dy < 0 { ndy = -1 }
	inp.mu.Lock()
	defer inp.mu.Unlock()
	C.cg_scroll(C.int(ndx), C.int(ndy))
}

// HandleGamepad — not supported on macOS without a virtual HID driver.
func (inp *Input) HandleGamepad(slot int, data []byte) {
	_ = slot
	_ = data
}

func boolToCInt(b bool) C.int {
	if b {
		return 1
	}
	return 0
}

// vkToMac translates a Windows VK code (what the browser sends) to a macOS
// virtual keycode (Carbon kVK_*). Returns -1 if unmapped.
func vkToMac(vk uint16) int {
	if k, ok := vkMacMap[vk]; ok {
		return k
	}
	return -1
}

// Carbon kVK_* values are stable. Numeric values from <Carbon/HIToolbox/Events.h>.
var vkMacMap = map[uint16]int{
	// Control
	0x08: 0x33,  // Backspace → kVK_Delete
	0x09: 0x30,  // Tab
	0x0D: 0x24,  // Enter → kVK_Return
	0x14: 0x39,  // CapsLock
	0x1B: 0x35,  // Escape
	0x20: 0x31,  // Space

	// Navigation
	0x21: 0x74,  // PageUp
	0x22: 0x79,  // PageDown
	0x23: 0x77,  // End
	0x24: 0x73,  // Home
	0x25: 0x7B,  // Left
	0x26: 0x7E,  // Up
	0x27: 0x7C,  // Right
	0x28: 0x7D,  // Down
	0x2D: 0x72,  // Insert / Help on Mac
	0x2E: 0x75,  // Delete (forward delete)

	// Numbers
	0x30: 0x1D, 0x31: 0x12, 0x32: 0x13, 0x33: 0x14, 0x34: 0x15,
	0x35: 0x17, 0x36: 0x16, 0x37: 0x1A, 0x38: 0x1C, 0x39: 0x19,

	// Letters A-Z
	0x41: 0x00, // A
	0x42: 0x0B, // B
	0x43: 0x08, // C
	0x44: 0x02, // D
	0x45: 0x0E, // E
	0x46: 0x03, // F
	0x47: 0x05, // G
	0x48: 0x04, // H
	0x49: 0x22, // I
	0x4A: 0x26, // J
	0x4B: 0x28, // K
	0x4C: 0x25, // L
	0x4D: 0x2E, // M
	0x4E: 0x2D, // N
	0x4F: 0x1F, // O
	0x50: 0x23, // P
	0x51: 0x0C, // Q
	0x52: 0x0F, // R
	0x53: 0x01, // S
	0x54: 0x11, // T
	0x55: 0x20, // U
	0x56: 0x09, // V
	0x57: 0x0D, // W
	0x58: 0x07, // X
	0x59: 0x10, // Y
	0x5A: 0x06, // Z

	// Meta (Command)
	0x5B: 0x37, // Left
	0x5C: 0x36, // Right

	// Function
	0x70: 0x7A, // F1
	0x71: 0x78, // F2
	0x72: 0x63, // F3
	0x73: 0x76, // F4
	0x74: 0x60, // F5
	0x75: 0x61, // F6
	0x76: 0x62, // F7
	0x77: 0x64, // F8
	0x78: 0x65, // F9
	0x79: 0x6D, // F10
	0x7A: 0x67, // F11
	0x7B: 0x6F, // F12

	// Modifiers
	0xA0: 0x38, // Left Shift
	0xA1: 0x3C, // Right Shift
	0xA2: 0x3B, // Left Ctrl
	0xA3: 0x3E, // Right Ctrl
	0xA4: 0x3A, // Left Alt → Option
	0xA5: 0x3D, // Right Alt → Option

	// Punctuation
	0xBA: 0x29, // ; :
	0xBB: 0x18, // = +
	0xBC: 0x2B, // , <
	0xBD: 0x1B, // - _
	0xBE: 0x2F, // . >
	0xBF: 0x2C, // / ?
	0xC0: 0x32, // ` ~
	0xDB: 0x21, // [ {
	0xDC: 0x2A, // \ |
	0xDD: 0x1E, // ] }
	0xDE: 0x27, // ' "
}
