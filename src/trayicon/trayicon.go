// Package trayicon embeds the four volume-level tray icons (muted, low, mid,
// high) and provides HICON loading helpers.
package trayicon

import (
	_ "embed"
	"unsafe"

	"golang.org/x/sys/windows"
)

//go:embed tray-muted-32.png
var mutedPNG []byte

//go:embed tray-low-32.png
var lowPNG []byte

//go:embed tray-mid-32.png
var midPNG []byte

//go:embed tray-high-32.png
var highPNG []byte

// Level identifies a volume level for the tray icon.
type Level int

const (
	Muted Level = iota
	Low
	Mid
	High
)

// PNG returns the embedded PNG bytes for the given level.
func PNG(level Level) []byte {
	switch level {
	case Muted:
		return mutedPNG
	case Low:
		return lowPNG
	case Mid:
		return midPNG
	case High:
		return highPNG
	}
	return highPNG
}

// Handle returns an HICON for the given volume level, loaded from the
// embedded PNG. The caller owns the icon and must destroy it with
// DestroyIcon when done (or cache it for the process lifetime).
func Handle(level Level) windows.Handle {
	data := PNG(level)
	user32 := windows.NewLazySystemDLL("user32.dll")
	createIconFromResourceEx := user32.NewProc("CreateIconFromResourceEx")

	h, _, _ := createIconFromResourceEx.Call(
		uintptr(unsafe.Pointer(&data[0])),
		uintptr(len(data)),
		1, // fIcon = TRUE
		0x00030000,
		0, 0,
		0,
	)
	if h == 0 {
		loadIcon := user32.NewProc("LoadIconW")
		h, _, _ = loadIcon.Call(0, 32512) // IDI_APPLICATION
	}
	return windows.Handle(h)
}