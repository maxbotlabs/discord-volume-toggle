// Package icon embeds the application icon and provides a Win32 HICON handle.
package icon

import (
	_ "embed"
	"unsafe"

	"golang.org/x/sys/windows"
)

//go:embed app.png
var PNG []byte

// Handle returns an HICON loaded from the embedded PNG. The caller owns it
// (destroy with DestroyIcon when done, or cache for the process lifetime).
//
// CreateIconFromResourceEx accepts PNG data directly (Windows Vista+).
func Handle() windows.Handle {
	user32 := windows.NewLazySystemDLL("user32.dll")
	createIconFromResourceEx := user32.NewProc("CreateIconFromResourceEx")

	h, _, _ := createIconFromResourceEx.Call(
		uintptr(unsafe.Pointer(&PNG[0])),
		uintptr(len(PNG)),
		1, // fIcon = TRUE
		0x00030000, // version
		0, 0,       // desired size (0 = default)
		0,          // flags (LR_DEFAULTCOLOR)
	)
	if h == 0 {
		// Fall back to the default application icon if loading fails.
		loadIcon := user32.NewProc("LoadIconW")
		h, _, _ = loadIcon.Call(0, 32512) // IDI_APPLICATION
	}
	return windows.Handle(h)
}