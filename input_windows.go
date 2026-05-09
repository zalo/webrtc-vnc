//go:build windows

package main

import (
	"encoding/binary"
	"log"
	"sync"
	"syscall"
	"unsafe"
)

// Windows SendInput backend. No cgo, just user32.SendInput via syscall.
//
// The browser sends Windows VK codes already, so the keyboard side is a
// straight passthrough. The mouse side normalizes browser-supplied
// coordinates into the SendInput "absolute virtual desktop" space (0..65535
// over the entire virtual screen).

var (
	user32           = syscall.NewLazyDLL("user32.dll")
	procSendInput    = user32.NewProc("SendInput")
	procGetMetrics   = user32.NewProc("GetSystemMetrics")
	procGetCursorPos = user32.NewProc("GetCursorPos")
	procSetCursorPos = user32.NewProc("SetCursorPos")
)

const (
	inputMouse    = 0
	inputKeyboard = 1

	// MOUSEINPUT.dwFlags
	mouseEventFAbsolute      = 0x8000
	mouseEventFVirtualDesk   = 0x4000
	mouseEventFMove          = 0x0001
	mouseEventFLeftDown      = 0x0002
	mouseEventFLeftUp        = 0x0004
	mouseEventFRightDown     = 0x0008
	mouseEventFRightUp       = 0x0010
	mouseEventFMiddleDown    = 0x0020
	mouseEventFMiddleUp      = 0x0040
	mouseEventFXDown         = 0x0080
	mouseEventFXUp           = 0x0100
	mouseEventFWheel         = 0x0800
	mouseEventFHWheel        = 0x1000

	// KEYBDINPUT.dwFlags
	keyEventFKeyUp   = 0x0002
	keyEventFScancode = 0x0008
	keyEventFExtended = 0x0001

	// SM_CXVIRTUALSCREEN, SM_CYVIRTUALSCREEN, SM_XVIRTUALSCREEN, SM_YVIRTUALSCREEN
	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79

	wheelDelta = 120
)

// INPUT union — we always size to mouseInput / keyboardInput which are
// the same width; we picked the larger of the two for safety.
type mouseInput struct {
	dx          int32
	dy          int32
	mouseData   uint32
	dwFlags     uint32
	time        uint32
	dwExtraInfo uintptr
}

type keyboardInput struct {
	wVk         uint16
	wScan       uint16
	dwFlags     uint32
	time        uint32
	dwExtraInfo uintptr
	_padding    [8]byte // pad to match MOUSEINPUT size on 64-bit
}

type rawInput struct {
	inputType uint32
	_pad      uint32 // alignment to 8 on 64-bit
	union     [32]byte
}

func sendOne(in rawInput) {
	procSendInput.Call(
		uintptr(1),
		uintptr(unsafe.Pointer(&in)),
		uintptr(unsafe.Sizeof(in)),
	)
}

func mouseEvent(dx, dy int32, data uint32, flags uint32) rawInput {
	var in rawInput
	in.inputType = inputMouse
	mi := mouseInput{
		dx: dx, dy: dy, mouseData: data, dwFlags: flags,
	}
	*(*mouseInput)(unsafe.Pointer(&in.union)) = mi
	return in
}

func keyEvent(vk uint16, flags uint32) rawInput {
	var in rawInput
	in.inputType = inputKeyboard
	ki := keyboardInput{wVk: vk, dwFlags: flags}
	*(*keyboardInput)(unsafe.Pointer(&in.union)) = ki
	return in
}

func getMetric(idx int) int32 {
	r, _, _ := procGetMetrics.Call(uintptr(idx))
	return int32(r)
}

// Input is the Windows implementation that satisfies the same call surface
// as the Linux/macOS Input types.
type Input struct {
	mu    sync.Mutex
	vsX   int32
	vsY   int32
	vsW   int32
	vsH   int32
}

func NewInput() (*Input, error) {
	in := &Input{
		vsX: getMetric(smXVirtualScreen),
		vsY: getMetric(smYVirtualScreen),
		vsW: getMetric(smCXVirtualScreen),
		vsH: getMetric(smCYVirtualScreen),
	}
	if in.vsW == 0 {
		in.vsW = 1
	}
	if in.vsH == 0 {
		in.vsH = 1
	}
	log.Printf("Input: Windows SendInput backend (virtual screen %dx%d at %d,%d)",
		in.vsW, in.vsH, in.vsX, in.vsY)
	return in, nil
}

func (inp *Input) Close() {}

// Gamepad emulation on Windows would require a virtual HID driver
// (ViGEmBus or similar). Not in scope.
func (inp *Input) EnsureGamepad(slot int) { _ = slot }

// HandleKeyboard: [0x02][keycode:u16le][modifiers:u8][pressed:u8]
func (inp *Input) HandleKeyboard(data []byte) {
	if len(data) < 5 {
		return
	}
	vk := binary.LittleEndian.Uint16(data[1:3])
	pressed := data[4] != 0

	flags := uint32(0)
	if !pressed {
		flags |= keyEventFKeyUp
	}
	if isExtendedVK(vk) {
		flags |= keyEventFExtended
	}

	inp.mu.Lock()
	defer inp.mu.Unlock()
	sendOne(keyEvent(vk, flags))
}

// HandleMouseMove: [0x03][flags:u8][x:u16le][y:u16le]
//   flags bit0 = absolute (0..65535 normalized)
//   else relative deltas (signed int16)
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
		// Browser sent 0..65535 normalized to the *primary monitor* — but
		// SendInput's MOUSEEVENTF_VIRTUALDESK uses the entire virtual screen.
		// Map normalized → primary screen pixels → virtual screen normalized.
		primaryW := getMetric(0) // SM_CXSCREEN
		primaryH := getMetric(1) // SM_CYSCREEN
		px := int32(int(primaryW) * x / 65535)
		py := int32(int(primaryH) * y / 65535)
		// Convert to virtual desktop normalized (0..65535)
		vx := int32(int(px-inp.vsX) * 65535 / int(inp.vsW))
		vy := int32(int(py-inp.vsY) * 65535 / int(inp.vsH))
		sendOne(mouseEvent(vx, vy, 0, mouseEventFMove|mouseEventFAbsolute|mouseEventFVirtualDesk))
	} else {
		dx := int32(int16(x))
		dy := int32(int16(y))
		sendOne(mouseEvent(dx, dy, 0, mouseEventFMove))
	}
}

// HandleMouseButton: [0x04][button:u8][pressed:u8]
func (inp *Input) HandleMouseButton(data []byte) {
	if len(data) < 3 {
		return
	}
	btn := data[1]
	down := data[2] != 0

	var flags uint32
	switch btn {
	case 0:
		if down {
			flags = mouseEventFLeftDown
		} else {
			flags = mouseEventFLeftUp
		}
	case 1:
		if down {
			flags = mouseEventFMiddleDown
		} else {
			flags = mouseEventFMiddleUp
		}
	case 2:
		if down {
			flags = mouseEventFRightDown
		} else {
			flags = mouseEventFRightUp
		}
	default:
		return
	}

	inp.mu.Lock()
	defer inp.mu.Unlock()
	sendOne(mouseEvent(0, 0, 0, flags))
}

// HandleMouseScroll: [0x05][reserved:u8][dx:i16le][dy:i16le]
func (inp *Input) HandleMouseScroll(data []byte) {
	if len(data) < 6 {
		return
	}
	dx := int16(binary.LittleEndian.Uint16(data[2:4]))
	dy := int16(binary.LittleEndian.Uint16(data[4:6]))

	inp.mu.Lock()
	defer inp.mu.Unlock()
	if dy != 0 {
		val := int32(wheelDelta)
		if dy < 0 {
			val = -val
		}
		sendOne(mouseEvent(0, 0, uint32(val), mouseEventFWheel))
	}
	if dx != 0 {
		val := int32(wheelDelta)
		if dx < 0 {
			val = -val
		}
		sendOne(mouseEvent(0, 0, uint32(val), mouseEventFHWheel))
	}
}

func (inp *Input) HandleGamepad(slot int, data []byte) {
	_ = slot
	_ = data
}

// isExtendedVK returns true for VKs that need MOUSEEVENTF_EXTENDED (arrow
// keys, navigation keys, numpad / cursor variants).
func isExtendedVK(vk uint16) bool {
	switch vk {
	case 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28, // PgUp/Dn, End, Home, Arrows
		0x2D, 0x2E,        // Insert, Delete
		0x90,              // NumLock
		0xA3, 0xA5,        // RCtrl, RAlt
		0x5B, 0x5C, 0x5D:  // LWin, RWin, Apps
		return true
	}
	return false
}
