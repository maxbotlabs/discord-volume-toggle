// Package hotkey registers a global hotkey via the Win32 RegisterHotKey API
// and delivers press events through a channel.
package hotkey

import (
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Modifier flags for RegisterHotKey.
const (
	ModAlt     = 0x0001
	ModControl = 0x0002
	ModShift   = 0x0004
	ModWin     = 0x0008
)

// Key represents a single keybind: a virtual-key code plus modifier flags.
type Key struct {
	VK   uint32 // virtual-key code (e.g. 0x56 = 'V')
	Mods uint32 // combination of Mod* flags
}

// String returns a human-readable description of the keybind.
func (k Key) String() string {
	s := ""
	if k.Mods&ModControl != 0 {
		s += "Ctrl+"
	}
	if k.Mods&ModAlt != 0 {
		s += "Alt+"
	}
	if k.Mods&ModShift != 0 {
		s += "Shift+"
	}
	if k.Mods&ModWin != 0 {
		s += "Win+"
	}
	s += vkName(k.VK)
	return s
}

// vkName returns a friendly name for common virtual-key codes.
func vkName(vk uint32) string {
	if vk >= 0x30 && vk <= 0x39 { // 0-9
		return string(rune('0' + vk - 0x30))
	}
	if vk >= 0x41 && vk <= 0x5A { // A-Z
		return string(rune('A' + vk - 0x41))
	}
	if vk >= 0x70 && vk <= 0x87 { // F1-F24
		return fmt.Sprintf("F%d", vk-0x70+1)
	}
	names := map[uint32]string{
		0x20: "Space",
		0x0D: "Enter",
		0x1B: "Esc",
		0x09: "Tab",
		0x08: "Backspace",
		0x2D: "Insert",
		0x2E: "Delete",
		0x24: "Home",
		0x23: "End",
		0x21: "PageUp",
		0x22: "PageDown",
		0x25: "Left",
		0x26: "Up",
		0x27: "Right",
		0x28: "Down",
		0x6A: "Numpad*",
		0x6B: "Numpad+",
		0x6D: "Numpad-",
		0x6E: "Numpad.",
		0x6F: "Numpad/",
		0x90: "NumLock",
		0x14: "CapsLock",
		0x2C: "PrintScreen",
		0x91: "ScrollLock",
		0x13: "Pause",
	}
	if n, ok := names[vk]; ok {
		return n
	}
	return fmt.Sprintf("VK_0x%X", vk)
}

// Manager owns a registered hotkey and delivers press events.
type Manager struct {
	mu      sync.Mutex
	hwnd    windows.HWND
	id      int32
	key     Key
	events  chan Key
	closed  bool
}

// NewManager creates a Manager. The window handle must be set via SetHWND
// before Register is called (the window is created after the manager).
func NewManager(hwnd windows.HWND) *Manager {
	return &Manager{
		hwnd:   hwnd,
		id:     1,
		events: make(chan Key, 8),
	}
}

// SetHWND sets the window handle that receives WM_HOTKEY messages. Call this
// after the window is created, before Register.
func (m *Manager) SetHWND(hwnd windows.HWND) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hwnd = hwnd
}

// Events returns the channel on which hotkey presses are delivered.
func (m *Manager) Events() <-chan Key {
	return m.events
}

// Register registers (or re-registers) the given keybind as a global hotkey.
func (m *Manager) Register(k Key) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Unregister any existing hotkey first.
	if m.key.VK != 0 {
		unregisterHotKey(m.hwnd, m.id)
	}

	user32 := windows.NewLazySystemDLL("user32.dll")
	registerHotKey := user32.NewProc("RegisterHotKey")

	ret, _, _ := registerHotKey.Call(
		uintptr(m.hwnd),
		uintptr(m.id),
		uintptr(k.Mods),
		uintptr(k.VK),
	)
	if ret == 0 {
		return fmt.Errorf("RegisterHotKey failed for %s (may already be in use)", k.String())
	}

	m.key = k
	return nil
}

// Unregister removes the current hotkey.
func (m *Manager) Unregister() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.key.VK != 0 {
		unregisterHotKey(m.hwnd, m.id)
		m.key = Key{}
	}
}

// HandleMessage should be called from the window message loop for WM_HOTKEY.
// It returns true if the message was a hotkey press.
func (m *Manager) HandleMessage(msg uint32, wParam, lParam uintptr) bool {
	const WM_HOTKEY = 0x0312
	if msg != WM_HOTKEY {
		return false
	}
	if int32(wParam) != m.id {
		return false
	}
	m.mu.Lock()
	k := m.key
	m.mu.Unlock()
	select {
	case m.events <- k:
	default:
	}
	return true
}

func unregisterHotKey(hwnd windows.HWND, id int32) {
	user32 := windows.NewLazySystemDLL("user32.dll")
	unregisterHotKey := user32.NewProc("UnregisterHotKey")
	unregisterHotKey.Call(uintptr(hwnd), uintptr(id))
}

// Ensure unsafe is referenced (used by callers for key capture).
var _ = unsafe.Sizeof(uintptr(0))
