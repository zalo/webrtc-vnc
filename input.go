package main

import (
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Linux input event constants
const (
	EV_SYN = 0x00
	EV_KEY = 0x01
	EV_REL = 0x02
	EV_ABS = 0x03

	SYN_REPORT = 0x00

	// Relative axes
	REL_X     = 0x00
	REL_Y     = 0x01
	REL_WHEEL = 0x08
	REL_HWHEEL = 0x06

	// Absolute axes
	ABS_X      = 0x00
	ABS_Y      = 0x01
	ABS_Z      = 0x02
	ABS_RX     = 0x03
	ABS_RY     = 0x04
	ABS_RZ     = 0x05
	ABS_HAT0X  = 0x10
	ABS_HAT0Y  = 0x11

	// Mouse buttons
	BTN_LEFT   = 0x110
	BTN_RIGHT  = 0x111
	BTN_MIDDLE = 0x112
	BTN_SIDE   = 0x113
	BTN_EXTRA  = 0x114

	// Gamepad buttons
	BTN_SOUTH  = 0x130
	BTN_EAST   = 0x131
	BTN_NORTH  = 0x133
	BTN_WEST   = 0x134
	BTN_TL     = 0x136
	BTN_TR     = 0x137
	BTN_SELECT = 0x13a
	BTN_START  = 0x13b
	BTN_MODE   = 0x13c
	BTN_THUMBL = 0x13d
	BTN_THUMBR = 0x13e

	// Key max
	KEY_MAX = 0x2ff
	ABS_MAX = 0x3f
	ABS_CNT = ABS_MAX + 1

	// uinput ioctls
	UINPUT_MAX_NAME_SIZE = 80
	UI_SET_EVBIT         = 0x40045564
	UI_SET_KEYBIT        = 0x40045565
	UI_SET_RELBIT        = 0x40045566
	UI_SET_ABSBIT        = 0x40045567
	UI_DEV_CREATE        = 0x5501
	UI_DEV_DESTROY       = 0x5502

	// Bus types
	BUS_USB     = 0x03
	BUS_VIRTUAL = 0x06
)

// inputEvent matches Linux struct input_event (64-bit)
type inputEvent struct {
	Time  [16]byte // struct timeval (16 bytes on 64-bit)
	Type  uint16
	Code  uint16
	Value int32
}

// uinputUserDev matches Linux struct uinput_user_dev
type uinputUserDev struct {
	Name         [UINPUT_MAX_NAME_SIZE]byte
	ID           inputID
	FFEffectsMax int32
	Absmax       [ABS_CNT]int32
	Absmin       [ABS_CNT]int32
	Absfuzz      [ABS_CNT]int32
	Absflat      [ABS_CNT]int32
}

type inputID struct {
	Bustype uint16
	Vendor  uint16
	Product uint16
	Version uint16
}

// Input manages virtual input devices via uinput.
type Input struct {
	keyboard *os.File
	pointer  *os.File // absolute positioning (touchscreen-like, no REL axes)
	scroll   *os.File // relative scroll wheel + buttons
	gamepads [4]*os.File

	kbMu      sync.Mutex
	mouseMu   sync.Mutex
	gamepadMu [4]sync.Mutex
}

func NewInput() (*Input, error) {
	inp := &Input{}

	var err error
	inp.keyboard, err = createKeyboard()
	if err != nil {
		return nil, fmt.Errorf("keyboard: %w", err)
	}

	inp.pointer, err = createAbsPointer()
	if err != nil {
		inp.keyboard.Close()
		return nil, fmt.Errorf("pointer: %w", err)
	}

	inp.scroll, err = createScrollDevice()
	if err != nil {
		inp.keyboard.Close()
		inp.pointer.Close()
		return nil, fmt.Errorf("scroll: %w", err)
	}

	log.Printf("Input devices created (keyboard + pointer + scroll)")
	return inp, nil
}

func (inp *Input) Close() {
	if inp.keyboard != nil {
		unix.IoctlSetInt(int(inp.keyboard.Fd()), UI_DEV_DESTROY, 0)
		inp.keyboard.Close()
	}
	if inp.pointer != nil {
		unix.IoctlSetInt(int(inp.pointer.Fd()), UI_DEV_DESTROY, 0)
		inp.pointer.Close()
	}
	if inp.scroll != nil {
		unix.IoctlSetInt(int(inp.scroll.Fd()), UI_DEV_DESTROY, 0)
		inp.scroll.Close()
	}
	for i, f := range inp.gamepads {
		if f != nil {
			unix.IoctlSetInt(int(f.Fd()), UI_DEV_DESTROY, 0)
			f.Close()
			inp.gamepads[i] = nil
		}
	}
}

func (inp *Input) EnsureGamepad(slot int) {
	if slot < 0 || slot >= 4 {
		return
	}
	inp.gamepadMu[slot].Lock()
	defer inp.gamepadMu[slot].Unlock()

	if inp.gamepads[slot] != nil {
		return
	}

	gp, err := createGamepad(slot)
	if err != nil {
		log.Printf("Failed to create gamepad %d: %v", slot, err)
		return
	}
	inp.gamepads[slot] = gp
	log.Printf("Virtual gamepad %d created", slot)
}

// HandleKeyboard processes a keyboard input message.
// Format: [0x02][keycode:u16le][modifiers:u8][pressed:u8]
func (inp *Input) HandleKeyboard(data []byte) {
	vkCode := binary.LittleEndian.Uint16(data[1:3])
	pressed := data[4] != 0

	linuxKey := vkToLinux(vkCode)
	if linuxKey == 0 {
		return
	}

	value := int32(0)
	if pressed {
		value = 1
	}

	inp.kbMu.Lock()
	defer inp.kbMu.Unlock()

	writeInputEvent(inp.keyboard, EV_KEY, uint16(linuxKey), value)
	writeInputEvent(inp.keyboard, EV_SYN, SYN_REPORT, 0)
}

// HandleMouseMove processes a mouse move message.
// Format: [0x03][flags:u8][x:u16le][y:u16le]
func (inp *Input) HandleMouseMove(data []byte) {
	flags := data[1]
	x := int(binary.LittleEndian.Uint16(data[2:4]))
	y := int(binary.LittleEndian.Uint16(data[4:6]))

	inp.mouseMu.Lock()
	defer inp.mouseMu.Unlock()

	if flags&0x01 != 0 {
		// Absolute mode (0-65535) — goes to the dedicated abs pointer
		writeInputEvent(inp.pointer, EV_ABS, ABS_X, int32(x))
		writeInputEvent(inp.pointer, EV_ABS, ABS_Y, int32(y))
		writeInputEvent(inp.pointer, EV_SYN, SYN_REPORT, 0)
	} else {
		// Relative mode — goes to scroll device (which has REL axes)
		writeInputEvent(inp.scroll, EV_REL, REL_X, int32(int16(x)))
		writeInputEvent(inp.scroll, EV_REL, REL_Y, int32(int16(y)))
		writeInputEvent(inp.scroll, EV_SYN, SYN_REPORT, 0)
	}
}

// HandleMouseButton processes a mouse button message.
// Format: [0x04][button:u8][pressed:u8]
func (inp *Input) HandleMouseButton(data []byte) {
	button := data[1]
	pressed := data[2] != 0

	var code uint16
	switch button {
	case 0:
		code = BTN_LEFT
	case 1:
		code = BTN_MIDDLE
	case 2:
		code = BTN_RIGHT
	case 3:
		code = BTN_SIDE
	case 4:
		code = BTN_EXTRA
	default:
		return
	}

	value := int32(0)
	if pressed {
		value = 1
	}

	inp.mouseMu.Lock()
	defer inp.mouseMu.Unlock()

	// Send button events on the abs pointer so clicks land at the right position
	writeInputEvent(inp.pointer, EV_KEY, code, value)
	writeInputEvent(inp.pointer, EV_SYN, SYN_REPORT, 0)
}

// HandleMouseScroll processes a mouse scroll message.
// Format: [0x05][reserved:u8][dx:i16le][dy:i16le]
func (inp *Input) HandleMouseScroll(data []byte) {
	dx := int16(binary.LittleEndian.Uint16(data[2:4]))
	dy := int16(binary.LittleEndian.Uint16(data[4:6]))

	inp.mouseMu.Lock()
	defer inp.mouseMu.Unlock()

	if dy != 0 {
		// Normalize scroll: browser sends large values, uinput expects small deltas
		val := int32(dy)
		if val > 0 {
			val = 1
		} else {
			val = -1
		}
		writeInputEvent(inp.scroll, EV_REL, REL_WHEEL, val)
	}
	if dx != 0 {
		val := int32(dx)
		if val > 0 {
			val = 1
		} else {
			val = -1
		}
		writeInputEvent(inp.scroll, EV_REL, REL_HWHEEL, val)
	}
	writeInputEvent(inp.scroll, EV_SYN, SYN_REPORT, 0)
}

// HandleGamepad processes a gamepad state message.
// Format: [0x01][slot:u8][buttons:u16le][lx:i16][ly:i16][rx:i16][ry:i16][lt:u8][rt:u8]
func (inp *Input) HandleGamepad(slot int, data []byte) {
	if slot < 0 || slot >= 4 || inp.gamepads[slot] == nil {
		return
	}

	buttons := binary.LittleEndian.Uint16(data[2:4])
	lx := int16(binary.LittleEndian.Uint16(data[4:6]))
	ly := int16(binary.LittleEndian.Uint16(data[6:8]))
	rx := int16(binary.LittleEndian.Uint16(data[8:10]))
	ry := int16(binary.LittleEndian.Uint16(data[10:12]))
	lt := data[12]
	rt := data[13]

	inp.gamepadMu[slot].Lock()
	defer inp.gamepadMu[slot].Unlock()

	f := inp.gamepads[slot]

	// Analog sticks
	writeInputEvent(f, EV_ABS, ABS_X, int32(lx))
	writeInputEvent(f, EV_ABS, ABS_Y, int32(ly))
	writeInputEvent(f, EV_ABS, ABS_RX, int32(rx))
	writeInputEvent(f, EV_ABS, ABS_RY, int32(ry))

	// Triggers
	writeInputEvent(f, EV_ABS, ABS_Z, int32(lt))
	writeInputEvent(f, EV_ABS, ABS_RZ, int32(rt))

	// D-pad as hat axes
	hatX := int32(0)
	if buttons&(1<<14) != 0 { // D-Left
		hatX = -1
	} else if buttons&(1<<15) != 0 { // D-Right
		hatX = 1
	}
	hatY := int32(0)
	if buttons&(1<<12) != 0 { // D-Up
		hatY = -1
	} else if buttons&(1<<13) != 0 { // D-Down
		hatY = 1
	}
	writeInputEvent(f, EV_ABS, ABS_HAT0X, hatX)
	writeInputEvent(f, EV_ABS, ABS_HAT0Y, hatY)

	// Face buttons and bumpers
	type btnMap struct {
		bit  int
		code uint16
	}
	btnMaps := []btnMap{
		{0, BTN_SOUTH},  // A
		{1, BTN_EAST},   // B
		{2, BTN_NORTH},  // X
		{3, BTN_WEST},   // Y
		{4, BTN_TL},     // LB
		{5, BTN_TR},     // RB
		{8, BTN_SELECT}, // Back
		{9, BTN_START},  // Start
		{10, BTN_THUMBL}, // L3
		{11, BTN_THUMBR}, // R3
	}

	for _, m := range btnMaps {
		val := int32(0)
		if buttons&(1<<m.bit) != 0 {
			val = 1
		}
		writeInputEvent(f, EV_KEY, m.code, val)
	}

	writeInputEvent(f, EV_SYN, SYN_REPORT, 0)
}

// Device creation helpers

func createKeyboard() (*os.File, error) {
	f, err := os.OpenFile("/dev/uinput", os.O_WRONLY, 0660)
	if err != nil {
		return nil, err
	}

	fd := int(f.Fd())

	// Set up event types
	if err := ioctlSetBit(fd, UI_SET_EVBIT, EV_KEY); err != nil {
		f.Close()
		return nil, err
	}

	// Enable all standard keys (0-255)
	for key := 1; key <= 255; key++ {
		ioctlSetBit(fd, UI_SET_KEYBIT, key)
	}

	// Write device info
	dev := uinputUserDev{}
	copy(dev.Name[:], "WebRTC-VNC Keyboard")
	dev.ID.Bustype = BUS_VIRTUAL
	dev.ID.Vendor = 0x1234
	dev.ID.Product = 0x0001
	dev.ID.Version = 1

	if err := writeDevInfo(f, dev); err != nil {
		f.Close()
		return nil, err
	}

	if err := ioctlCreate(fd); err != nil {
		f.Close()
		return nil, err
	}

	return f, nil
}

// createAbsPointer creates a touchscreen-like absolute pointing device.
// Must NOT have any REL axes — libinput treats devices with both ABS+REL as relative mice.
func createAbsPointer() (*os.File, error) {
	f, err := os.OpenFile("/dev/uinput", os.O_WRONLY, 0660)
	if err != nil {
		return nil, err
	}

	fd := int(f.Fd())

	ioctlSetBit(fd, UI_SET_EVBIT, EV_KEY)
	ioctlSetBit(fd, UI_SET_EVBIT, EV_ABS)

	// Mouse buttons on the abs device so clicks happen at the abs position
	ioctlSetBit(fd, UI_SET_KEYBIT, BTN_LEFT)
	ioctlSetBit(fd, UI_SET_KEYBIT, BTN_RIGHT)
	ioctlSetBit(fd, UI_SET_KEYBIT, BTN_MIDDLE)
	ioctlSetBit(fd, UI_SET_KEYBIT, BTN_SIDE)
	ioctlSetBit(fd, UI_SET_KEYBIT, BTN_EXTRA)

	// Use BTN_TOUCH to hint that this is a direct touch/tablet device
	ioctlSetBit(fd, UI_SET_KEYBIT, 0x14a) // BTN_TOUCH

	// Absolute axes only
	ioctlSetBit(fd, UI_SET_ABSBIT, ABS_X)
	ioctlSetBit(fd, UI_SET_ABSBIT, ABS_Y)

	dev := uinputUserDev{}
	copy(dev.Name[:], "WebRTC-VNC Pointer")
	dev.ID.Bustype = BUS_VIRTUAL
	dev.ID.Vendor = 0x1234
	dev.ID.Product = 0x0002
	dev.ID.Version = 1

	dev.Absmax[ABS_X] = 65535
	dev.Absmax[ABS_Y] = 65535

	if err := writeDevInfo(f, dev); err != nil {
		f.Close()
		return nil, err
	}

	if err := ioctlCreate(fd); err != nil {
		f.Close()
		return nil, err
	}

	return f, nil
}

// createScrollDevice creates a relative-only device for scroll wheel events.
func createScrollDevice() (*os.File, error) {
	f, err := os.OpenFile("/dev/uinput", os.O_WRONLY, 0660)
	if err != nil {
		return nil, err
	}

	fd := int(f.Fd())

	ioctlSetBit(fd, UI_SET_EVBIT, EV_REL)

	ioctlSetBit(fd, UI_SET_RELBIT, REL_X)
	ioctlSetBit(fd, UI_SET_RELBIT, REL_Y)
	ioctlSetBit(fd, UI_SET_RELBIT, REL_WHEEL)
	ioctlSetBit(fd, UI_SET_RELBIT, REL_HWHEEL)

	dev := uinputUserDev{}
	copy(dev.Name[:], "WebRTC-VNC Scroll")
	dev.ID.Bustype = BUS_VIRTUAL
	dev.ID.Vendor = 0x1234
	dev.ID.Product = 0x0003
	dev.ID.Version = 1

	if err := writeDevInfo(f, dev); err != nil {
		f.Close()
		return nil, err
	}

	if err := ioctlCreate(fd); err != nil {
		f.Close()
		return nil, err
	}

	return f, nil
}

func createGamepad(slot int) (*os.File, error) {
	f, err := os.OpenFile("/dev/uinput", os.O_WRONLY, 0660)
	if err != nil {
		return nil, err
	}

	fd := int(f.Fd())

	// Event types
	ioctlSetBit(fd, UI_SET_EVBIT, EV_KEY)
	ioctlSetBit(fd, UI_SET_EVBIT, EV_ABS)

	// Buttons
	gamepadButtons := []int{
		BTN_SOUTH, BTN_EAST, BTN_NORTH, BTN_WEST,
		BTN_TL, BTN_TR, BTN_SELECT, BTN_START,
		BTN_MODE, BTN_THUMBL, BTN_THUMBR,
	}
	for _, btn := range gamepadButtons {
		ioctlSetBit(fd, UI_SET_KEYBIT, btn)
	}

	// Absolute axes
	absAxes := []int{ABS_X, ABS_Y, ABS_RX, ABS_RY, ABS_Z, ABS_RZ, ABS_HAT0X, ABS_HAT0Y}
	for _, axis := range absAxes {
		ioctlSetBit(fd, UI_SET_ABSBIT, axis)
	}

	dev := uinputUserDev{}
	copy(dev.Name[:], fmt.Sprintf("WebRTC-VNC Gamepad %d", slot+1))
	dev.ID.Bustype = BUS_USB
	dev.ID.Vendor = 0x045e  // Microsoft
	dev.ID.Product = 0x028e // Xbox 360 Controller
	dev.ID.Version = 1

	// Stick axes: -32768 to 32767
	for _, axis := range []int{ABS_X, ABS_Y, ABS_RX, ABS_RY} {
		dev.Absmin[axis] = -32768
		dev.Absmax[axis] = 32767
		dev.Absfuzz[axis] = 16
		dev.Absflat[axis] = 128
	}

	// Trigger axes: 0-255
	dev.Absmax[ABS_Z] = 255
	dev.Absmax[ABS_RZ] = 255

	// D-pad hat: -1 to 1
	dev.Absmin[ABS_HAT0X] = -1
	dev.Absmax[ABS_HAT0X] = 1
	dev.Absmin[ABS_HAT0Y] = -1
	dev.Absmax[ABS_HAT0Y] = 1

	if err := writeDevInfo(f, dev); err != nil {
		f.Close()
		return nil, err
	}

	if err := ioctlCreate(fd); err != nil {
		f.Close()
		return nil, err
	}

	return f, nil
}

// Low-level helpers

func writeInputEvent(f *os.File, typ, code uint16, value int32) {
	ev := inputEvent{
		Type:  typ,
		Code:  code,
		Value: value,
	}
	buf := (*[unsafe.Sizeof(ev)]byte)(unsafe.Pointer(&ev))[:]
	f.Write(buf)
}

func writeDevInfo(f *os.File, dev uinputUserDev) error {
	buf := (*[unsafe.Sizeof(dev)]byte)(unsafe.Pointer(&dev))[:]
	_, err := f.Write(buf)
	return err
}

func ioctlSetBit(fd int, request, bit int) error {
	return unix.IoctlSetInt(fd, uint(request), bit)
}

func ioctlCreate(fd int) error {
	return unix.IoctlSetInt(fd, UI_DEV_CREATE, 0)
}

// VK code to Linux key code mapping

func vkToLinux(vk uint16) int {
	if code, ok := vkMap[vk]; ok {
		return code
	}
	return 0
}

var vkMap = map[uint16]int{
	// Control keys
	0x08: 14,  // Backspace
	0x09: 15,  // Tab
	0x0D: 28,  // Enter
	0x13: 119, // Pause
	0x14: 58,  // CapsLock
	0x1B: 1,   // Escape
	0x20: 57,  // Space

	// Navigation
	0x21: 104, // PageUp
	0x22: 109, // PageDown
	0x23: 107, // End
	0x24: 102, // Home
	0x25: 105, // Left
	0x26: 103, // Up
	0x27: 106, // Right
	0x28: 108, // Down
	0x2C: 99,  // PrintScreen
	0x2D: 110, // Insert
	0x2E: 111, // Delete

	// Numbers 0-9
	0x30: 11, // 0
	0x31: 2,  // 1
	0x32: 3,  // 2
	0x33: 4,  // 3
	0x34: 5,  // 4
	0x35: 6,  // 5
	0x36: 7,  // 6
	0x37: 8,  // 7
	0x38: 9,  // 8
	0x39: 10, // 9

	// Letters A-Z
	0x41: 30, // A
	0x42: 48, // B
	0x43: 46, // C
	0x44: 32, // D
	0x45: 18, // E
	0x46: 33, // F
	0x47: 34, // G
	0x48: 35, // H
	0x49: 23, // I
	0x4A: 36, // J
	0x4B: 37, // K
	0x4C: 38, // L
	0x4D: 50, // M
	0x4E: 49, // N
	0x4F: 24, // O
	0x50: 25, // P
	0x51: 16, // Q
	0x52: 19, // R
	0x53: 31, // S
	0x54: 20, // T
	0x55: 22, // U
	0x56: 47, // V
	0x57: 17, // W
	0x58: 45, // X
	0x59: 21, // Y
	0x5A: 44, // Z

	// Windows keys
	0x5B: 125, // Left Meta
	0x5C: 126, // Right Meta

	// Function keys
	0x70: 59, // F1
	0x71: 60, // F2
	0x72: 61, // F3
	0x73: 62, // F4
	0x74: 63, // F5
	0x75: 64, // F6
	0x76: 65, // F7
	0x77: 66, // F8
	0x78: 67, // F9
	0x79: 68, // F10
	0x7A: 87, // F11
	0x7B: 88, // F12

	// Lock keys
	0x90: 69, // NumLock
	0x91: 70, // ScrollLock

	// Modifier keys
	0xA0: 42,  // Left Shift
	0xA1: 54,  // Right Shift
	0xA2: 29,  // Left Ctrl
	0xA3: 97,  // Right Ctrl
	0xA4: 56,  // Left Alt
	0xA5: 100, // Right Alt

	// OEM keys (punctuation)
	0xBA: 39, // ; :
	0xBB: 13, // = +
	0xBC: 51, // , <
	0xBD: 12, // - _
	0xBE: 52, // . >
	0xBF: 53, // / ?
	0xC0: 41, // ` ~
	0xDB: 26, // [ {
	0xDC: 43, // \ |
	0xDD: 27, // ] }
	0xDE: 40, // ' "
}
