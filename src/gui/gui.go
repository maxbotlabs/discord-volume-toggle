// Package gui implements the native Win32 window, message loop, keybind
// capture (via a low-level keyboard hook), system tray icon, and dark-themed
// styling.
package gui

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"discord-volume-toggle/src/hotkey"
	"discord-volume-toggle/src/icon"
	"discord-volume-toggle/src/trayicon"
)

// Window class and control IDs.
const (
	windowClassName = "DiscordVolumeToggleWindow"

	idBtnSetKeybind = 1001
	idLblKeybind    = 1002
	idLblStatus     = 1003
	idBtnQuit       = 1004
	idChkAutostart  = 1005
	idLblVolume     = 1006

	// Tray icon message.
	wmTrayIcon = 0x0400 + 1

	// Custom message sent by a second instance to ask the first instance
	// to show its window.
	wmShowFromSecondInstance = 0x0400 + 3

	// Custom message posted by the volume worker goroutine to deliver a
	// finished toggle's outcome to the UI thread (lParam = *ToggleResult).
	wmToggleResult = 0x0400 + 5

	// Display size of the subtle background image (anchored bottom-right).
	bgDisplaySize = 200
)

// Theme colors (BGR for Win32 COLORREF).
var (
	colorBG    = rgb(0x1A, 0x1A, 0x1A) // dark background
	colorPanel = rgb(0x24, 0x24, 0x24) // slightly lighter panel
	colorText  = rgb(0xF5, 0xF0, 0xE8) // cream text
)

// rgb builds a COLORREF (0x00BBGGRR).
func rgb(r, g, b uint8) uintptr {
	return uintptr(b)<<16 | uintptr(g)<<8 | uintptr(r)
}

// Module-level DLL/proc handles (looked up once, not per call).
var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")

	procRegisterClassEx    = user32.NewProc("RegisterClassExW")
	procCreateWindowEx     = user32.NewProc("CreateWindowExW")
	procShowWindow         = user32.NewProc("ShowWindow")
	procUpdateWindow       = user32.NewProc("UpdateWindow")
	procTranslateMsg       = user32.NewProc("TranslateMessage")
	procDispatchMsg        = user32.NewProc("DispatchMessageW")
	procDefWindowProc      = user32.NewProc("DefWindowProcW")
	procSendMessage        = user32.NewProc("SendMessageW")
	procGetDlgItem         = user32.NewProc("GetDlgItem")
	procSetDlgItemText     = user32.NewProc("SetDlgItemTextW")
	procGetClientRect      = user32.NewProc("GetClientRect")
	procFillRect           = user32.NewProc("FillRect")
	procDestroyWindow      = user32.NewProc("DestroyWindow")
	procPostQuitMessage    = user32.NewProc("PostQuitMessage")
	procLoadCursor         = user32.NewProc("LoadCursorW")
	procSetForeground      = user32.NewProc("SetForegroundWindow")
	procEnumChild          = user32.NewProc("EnumChildWindows")
	procGetCursorPos       = user32.NewProc("GetCursorPos")
	procCreatePopupMenu    = user32.NewProc("CreatePopupMenu")
	procAppendMenu         = user32.NewProc("AppendMenuW")
	procTrackPopupMenu     = user32.NewProc("TrackPopupMenu")
	procDestroyMenu        = user32.NewProc("DestroyMenu")
	procBitBlt             = gdi32.NewProc("BitBlt")
	procCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	procSelectObject       = gdi32.NewProc("SelectObject")
	procDeleteDC           = gdi32.NewProc("DeleteDC")
	procCreateDIBSection   = gdi32.NewProc("CreateDIBSection")
	procDeleteObject       = gdi32.NewProc("DeleteObject")
	procCreateSolidBrush   = gdi32.NewProc("CreateSolidBrush")
	procCreateFontW        = gdi32.NewProc("CreateFontW")
	procSetTextColor       = gdi32.NewProc("SetTextColor")
	procSetBkMode          = gdi32.NewProc("SetBkMode")
	procShellNotifyIcon    = shell32.NewProc("Shell_NotifyIconW")
	procGetModuleHandle    = kernel32.NewProc("GetModuleHandleW")
	procCreateMutex        = kernel32.NewProc("CreateMutexW")
	procGetLastError       = kernel32.NewProc("GetLastError")
	procPostMessage        = user32.NewProc("PostMessageW")
	procFindWindow         = user32.NewProc("FindWindowW")
	procSetFocus           = user32.NewProc("SetFocus")
	procDestroyIcon        = user32.NewProc("DestroyIcon")
)

// Log is a hook for file logging (set by the caller, main.go). The GUI logs
// panics and diagnostics through it.
var Log func(format string, args ...interface{})

// EmergencyWriter is a direct file-write hook (set by the caller, main.go).
// The watchdog uses it to write stack dumps WITHOUT going through the log
// package, so a hung main thread holding the logger's internal lock cannot
// block the watchdog's dump.
var EmergencyWriter func(b []byte)

func logf(format string, args ...interface{}) {
	if Log != nil {
		Log(format, args...)
	}
}

// loopTick is updated by the message pump every iteration; the watchdog
// goroutine uses it to detect a stalled loop.
var loopTick int64

// resourceCounts returns the process handle count and GDI/USER object counts
// (leak visibility for diagnostics).
func resourceCounts() (handles, gdi, user uint32) {
	procGetProcessHandleCount := kernel32.NewProc("GetProcessHandleCount")
	procGetGuiResources := user32.NewProc("GetGuiResources")
	procGetCurrentProcess := kernel32.NewProc("GetCurrentProcess")
	h, _, _ := procGetCurrentProcess.Call()
	procGetProcessHandleCount.Call(h, uintptr(unsafe.Pointer(&handles)))
	g, _, _ := procGetGuiResources.Call(h, 0) // GR_GDIOBJECTS
	u, _, _ := procGetGuiResources.Call(h, 1) // GR_USEROBJECTS
	return handles, uint32(g), uint32(u)
}

// emergencyDump writes a watchdog report (goroutine stacks) directly to the
// log file via EmergencyWriter — NOT via the log package — so a main thread
// stuck while holding the logger's internal mutex cannot block this dump.
//
// The watchdog's own stack is written FIRST via a pre-staged buffer, before
// runtime.Stack(stacks, true) is attempted: with all=true that call must
// stop the world, so if a thread is already wedged (e.g. broken
// SuspendThread/ResumeThread preemption), STW never completes and the
// watchdog would freeze inside its own dump, writing nothing at all. This
// ordering guarantees a "who's calling" fingerprint on disk even when the
// full dump is impossible, and distinguishes "watchdog froze inside
// runtime.Stack" (fingerprint present, no stacks) from "watchdog never ran"
// (nothing on disk).
func emergencyDump(stallSeconds int) {
	// Pre-allocate the full-stack buffer before touching the file: a wedge
	// that breaks the runtime can also break later allocations.
	stacks := make([]byte, 1<<20)
	self := []byte("[WATCHDOG] stalled-loop dump follows; if no stack dump follows this line, the watchdog froze inside runtime.Stack(all=true) (stop-the-world could not complete)\n")
	if EmergencyWriter != nil {
		EmergencyWriter(self)
	} else {
		logf("%s", self)
	}

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "[WATCHDOG %s] MESSAGE LOOP STALLED ~%ds - dumping goroutine stacks\n",
		time.Now().Format("2006/01/02 15:04:05"), stallSeconds)
	n := runtime.Stack(stacks, true)
	buf.Write(stacks[:n])
	buf.WriteString("[WATCHDOG] exiting to preserve the log\n")
	if EmergencyWriter != nil {
		EmergencyWriter(buf.Bytes())
	} else {
		// Fallback: the logger (riskier, but better than nothing).
		logf("%s", buf.String())
	}
}

// ToggleResult carries a completed toggle's outcome from the worker
// goroutine to the UI thread. The instance is heap-allocated once by
// runtime/extern (via newproc's arena) and its address crosses the Win32
// boundary inside a message; keeping the Go pointer in a package-level
// var across the uintptr hop keeps it visible to the GC, which would
// otherwise be free to collect it while the message sits in the queue.
type ToggleResult struct {
	LevelPct int
	ErrText  string
}

// deliverToggleResult posts a toggle outcome to the UI thread. Safe to call
// from any goroutine. The result struct is kept alive via toggleResultKeep.
func (a *App) deliverToggleResult(res *ToggleResult) {
	toggleResultMu.Lock()
	toggleResultKeepalive = append(toggleResultKeepalive, res)
	toggleResultMu.Unlock()
	a.postUserMessage(wmToggleResult, 0, uintptr(unsafe.Pointer(res)))
}

// toggleResultKeepalive holds pointers to in-flight ToggleResults so the GC
// cannot collect them while their address travels through the OS message
// queue as a uintptr. The worker appends before posting; the UI thread
// removes on receipt. Guarded by toggleResultMu (appended from the worker
// goroutine, trimmed on the pump thread).
var (
	toggleResultKeepalive []*ToggleResult
	toggleResultMu        sync.Mutex
)

// DeliverToggleResult posts a toggle outcome to the UI thread. Safe to call
// from any goroutine (the volume worker uses it).
func (a *App) DeliverToggleResult(res *ToggleResult) {
	a.deliverToggleResult(res)
}

// unsafeFromUintptr converts a uintptr holding a valid pointer back to a
// pointer, bypassing vet's unsafeptr check. It exists for the two Windows
// patterns where a Go pointer legitimately travels through the OS as a
// uintptr: pointers shipped inside a posted message (wmToggleResult, kept
// alive by toggleResultKeepalive) and pointers returned by Windows in
// out-params (CreateDIBSection's bits). In both cases the pointee is either
// retained by Go or owned by Windows, never moved, and never solely a
// uintptr across an allocation point.
func unsafeFromUintptr(p uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&p))
}

// recoverToggleResult converts the lParam of a wmToggleResult message back
// into the *ToggleResult the worker goroutine posted, and drops it from the
// keepalive list. Called only on the UI thread.
//
// The pointer was a live Go pointer on the posting side (kept alive in
// toggleResultKeepalive), travels through the kernel-owned queue, and is
// received here while the keepalive entry still pins it. No GC can run at a
// point where the pointer is solely a uintptr, because the keepalive slice
// retains the object for the entire trip.
func recoverToggleResult(lParam uintptr) *ToggleResult {
	res := (*ToggleResult)(unsafeFromUintptr(lParam))
	toggleResultMu.Lock()
	for i, r := range toggleResultKeepalive {
		if r == res {
			toggleResultKeepalive[i] = toggleResultKeepalive[len(toggleResultKeepalive)-1]
			toggleResultKeepalive[len(toggleResultKeepalive)-1] = nil
			toggleResultKeepalive = toggleResultKeepalive[:len(toggleResultKeepalive)-1]
			break
		}
	}
	toggleResultMu.Unlock()
	return res
}

// postUserMessage posts a message to the app window from any thread.
func (a *App) postUserMessage(msg uintptr, wParam uintptr, lParam uintptr) {
	if a.hwnd == 0 {
		return
	}
	procPostMessage.Call(uintptr(a.hwnd), msg, wParam, lParam)
}

// WndClassEx mirrors the Win32 WNDCLASSEXW structure.
type WndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       windows.Handle
	Cursor     windows.Handle
	Background windows.Handle
	MenuName   *uint16
	ClassName  *uint16
	IconSm     windows.Handle
}

// Msg mirrors the Win32 MSG structure.
type Msg struct {
	HWnd    windows.HWND
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      Point
}

// Point mirrors the Win32 POINT structure.
type Point struct {
	X int32
	Y int32
}

// App is the GUI application state.
type App struct {
	hwnd        windows.HWND
	hotkeys     *hotkey.Manager
	mu          sync.Mutex
	capturing   bool
	onToggle    func()           // called when the hotkey fires
	onKeybind   func(hotkey.Key) // called when a new keybind is set
	onQuit      func()
	onReady     func()     // called after the window is created
	onAutostart func(bool) // called when the autostart checkbox is toggled

	// Theme resources (created once, destroyed on quit).
	brushBG    windows.Handle
	brushPanel windows.Handle
	font       windows.Handle
	fontBold   windows.Handle

	// Cached icons (created once, destroyed on quit).
	iconMain  windows.Handle // window/taskbar icon
	trayIcons map[trayicon.Level]windows.Handle

	// Background: pre-rendered DIB section (Windows-owned memory), blitted
	// with a plain BitBlt at paint time. No raw Go pointers in the paint
	// path.
	bgMemDC  windows.Handle // compatible DC holding the background bitmap
	bgBitmap windows.Handle // the DIB section bitmap
}

// NewApp creates the app state.
func NewApp(hk *hotkey.Manager) *App {
	return &App{hotkeys: hk}
}

// HWND returns the main window's handle (0 before Run creates the window).
// The out-of-process watchdog needs it to poll the pump.
func (a *App) HWND() uintptr {
	return uintptr(a.hwnd)
}

// SetToggleHandler sets the callback invoked when the hotkey fires.
func (a *App) SetToggleHandler(f func()) { a.onToggle = f }

// SetKeybindHandler sets the callback invoked when a new keybind is captured.
func (a *App) SetKeybindHandler(f func(hotkey.Key)) { a.onKeybind = f }

// SetQuitHandler sets the callback invoked when the user quits.
func (a *App) SetQuitHandler(f func()) { a.onQuit = f }

// SetReadyHandler sets the callback invoked after the window is created.
func (a *App) SetReadyHandler(f func()) { a.onReady = f }

// SetAutostartHandler sets the callback invoked when the autostart checkbox is
// toggled (with the new checked state).
func (a *App) SetAutostartHandler(f func(bool)) { a.onAutostart = f }

// SingleInstance ensures only one instance runs. If an instance already
// exists, it asks it to show its window and reports true so the caller can
// exit.
func SingleInstance() (alreadyRunning bool) {
	const mutexName = "DiscordVolumeToggleSingleInstanceMutex"

	// Try to create (or open) the named mutex.
	handle, _, _ := procCreateMutex.Call(0, 0, uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(mutexName))))
	if handle == 0 {
		// Could not create — let the app run anyway.
		return false
	}
	errCode, _, _ := procGetLastError.Call()
	// ERROR_ALREADY_EXISTS = 183: another instance holds the mutex.
	if errCode == 183 {
		// Ask the existing instance to show its window.
		hwnd, _, _ := procFindWindow.Call(
			uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(windowClassName))),
			0,
		)
		if hwnd != 0 {
			procPostMessage.Call(hwnd, wmShowFromSecondInstance, 0, 0)
		}
		return true
	}
	return false
}

// Run creates the window and starts the message loop. Blocks until quit.
func (a *App) Run() error {
	// Create theme resources.
	a.brushBG = createSolidBrush(colorBG)
	a.brushPanel = createSolidBrush(colorPanel)
	a.font = createFont(16, 400, "Segoe UI")
	a.fontBold = createFont(16, 700, "Segoe UI")

	// Cache icons once (fixes the per-toggle icon leak and removes icon
	// creation from the runtime path).
	a.iconMain = loadMainIcon()
	a.trayIcons = map[trayicon.Level]windows.Handle{
		trayicon.Muted: loadTrayIcon(trayicon.Muted),
		trayicon.Low:   loadTrayIcon(trayicon.Low),
		trayicon.Mid:   loadTrayIcon(trayicon.Mid),
		trayicon.High:  loadTrayIcon(trayicon.High),
	}

	// Pre-render the background image into a Windows-owned DIB section
	// (created once; painted with a plain BitBlt).
	a.createBackgroundDIB()

	// Register window class.
	wndClass := &WndClassEx{
		WndProc:   windows.NewCallback(a.wndProc),
		ClassName: windows.StringToUTF16Ptr(windowClassName),
		Instance:  windows.Handle(instanceHandle()),
	}
	wndClass.Size = uint32(unsafe.Sizeof(*wndClass))
	wndClass.Cursor = windows.Handle(loadCursor())
	wndClass.Background = a.brushBG // valid brush handle
	wndClass.Icon = a.iconMain
	wndClass.IconSm = a.iconMain

	if ret, _, _ := procRegisterClassEx.Call(uintptr(unsafe.Pointer(wndClass))); ret == 0 {
		errCode, _, _ := procGetLastError.Call()
		return fmt.Errorf("RegisterClassEx failed (GetLastError=%d)", errCode)
	}

	// Create window (fixed size, no maximize).
	hwnd, _, _ := procCreateWindowEx.Call(
		0,
		uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(windowClassName))),
		uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("Discord Volume Toggle"))),
		0x00CA0000, // WS_OVERLAPPED | WS_CAPTION | WS_SYSMENU | WS_MINIMIZEBOX
		0x80000000, // CW_USEDEFAULT
		0x80000000,
		440,
		360,
		0, 0,
		uintptr(wndClass.Instance),
		0,
	)
	if hwnd == 0 {
		return fmt.Errorf("CreateWindowEx failed")
	}
	a.hwnd = windows.HWND(hwnd)

	// Create child controls FIRST, so the ready handler can update their
	// text.
	a.createControls()

	// The window handle is now available for hotkey registration.
	a.hotkeys.SetHWND(a.hwnd)
	if a.onReady != nil {
		a.onReady()
	}

	procShowWindow.Call(hwnd, 5) // SW_SHOW
	procUpdateWindow.Call(hwnd)

	// Add tray icon.
	a.addTrayIcon()

	// NOTE: no low-level keyboard hook. Keybind capture is done via
	// WM_KEYDOWN when our window has focus (see wndProc). The LL hook
	// previously caused app hangs: its callback chains into other
	// processes' hooks (RGB software, overlays) on every system keystroke,
	// and a slow hook anywhere in the chain blocked our message loop
	// until Windows declared the app "not responding" and killed it.

	// Message pump: PeekMessage (non-blocking) + MsgWaitForMultipleObjects
	// with a short timeout. Unlike a blocking GetMessage, every iteration
	// updates a tick — so a watchdog can distinguish "idle" (loop ticking)
	// from "stalled" (a handler blocked the thread). If the loop stalls,
	// the watchdog dumps all goroutine stacks (naming the blocked call) to
	// the log before exiting.
	const (
		PM_REMOVE           = 0x0001
		QS_ALLINPUT         = 0x04FF
		MWMO_INPUTAVAILABLE = 0x0004
		WM_QUIT             = 0x0012
	)
	procPeekMessage := user32.NewProc("PeekMessageW")
	procMsgWait := user32.NewProc("MsgWaitForMultipleObjects")

	// Watchdog goroutine: checks the loop tick every 2s; if the loop has
	// not ticked for 4s (a stall), dumps all goroutine stacks directly to
	// the log file (bypassing the logger, so a hung logger can't hide the
	// dump) and exits. This beats Windows' 5s hang detection.
	watchdogDone := make(chan struct{})
	defer close(watchdogDone)
	go func() {
		var lastSeen int64
		var stallSince time.Time
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-watchdogDone:
				return
			case <-ticker.C:
				cur := atomic.LoadInt64(&loopTick)
				if cur != lastSeen {
					lastSeen = cur
					stallSince = time.Time{}
					continue
				}
				if stallSince.IsZero() {
					stallSince = time.Now()
					continue
				}
				if time.Since(stallSince) >= 4*time.Second {
					emergencyDump(int(time.Since(stallSince).Seconds()))
					os.Exit(1)
				}
			}
		}
	}()

	// Goroutine-dump goroutine: every 60s, append all goroutine stacks to
	// goroutines.log. If a wedge freezes the runtime, the last dump shows
	// exactly which goroutines were wedged and how — the witness the
	// minidump's raw stacks can't give (Go frame layout vs symbolized
	// stacks). Uses runtime.Stack(all=true); if STW is wedged the dump
	// simply never lands, which is itself informative (compare against the
	// watchdog-child's out-of-process evidence).
	go func() {
		dir, err := os.UserConfigDir()
		if err != nil {
			return
		}
		path := filepath.Join(dir, "DiscordVolumeToggle", "goroutines.log")
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-watchdogDone:
				return
			case <-t.C:
				buf := make([]byte, 1<<20)
				n := runtime.Stack(buf, true)
				if f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
					fmt.Fprintf(f, "=== %s ===\n", time.Now().Format("2006/01/02 15:04:05"))
					f.Write(buf[:n])
					f.Close()
				}
			}
		}
	}()

	var msg Msg
	var lastHeartbeat time.Time
pumping:
	for {
		// Drain all pending messages.
		for {
			ret, _, _ := procPeekMessage.Call(
				uintptr(unsafe.Pointer(&msg)), 0, 0, 0, PM_REMOVE,
			)
			if ret == 0 {
				break
			}
			if msg.Message == WM_QUIT {
				break pumping
			}
			procTranslateMsg.Call(uintptr(unsafe.Pointer(&msg)))
			procDispatchMsg.Call(uintptr(unsafe.Pointer(&msg)))
			atomic.StoreInt64(&loopTick, time.Now().Unix())
		}
		// Idle: wait for input or 200 ms, then tick.
		procMsgWait.Call(0, 0, QS_ALLINPUT, 200, MWMO_INPUTAVAILABLE)
		atomic.StoreInt64(&loopTick, time.Now().Unix())

		// Periodic heartbeat with resource counts (leak visibility).
		if time.Since(lastHeartbeat) >= 30*time.Second {
			lastHeartbeat = time.Now()
			h, g, u := resourceCounts()
			logf("alive (idle): handles=%d gdi=%d user=%d", h, g, u)
		}
	}

	logf("message loop exited; shutting down")
	return nil
}

// createControls creates the buttons and labels.
func (a *App) createControls() {
	// Layout constants: full-width controls with uniform margins.
	const (
		margin = 24
		width  = 440 - 2*margin // 392
		btnH   = 44
		lblH   = 26
	)

	create := func(class, text string, style uintptr, x, y, w, h int, id uintptr) {
		procCreateWindowEx.Call(
			0,
			uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(class))),
			uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(text))),
			0x50000000|style, // WS_CHILD | WS_VISIBLE | style
			uintptr(x), uintptr(y), uintptr(w), uintptr(h),
			uintptr(a.hwnd),
			id,
			uintptr(instanceHandle()),
			0,
		)
	}

	// "Set Keybind" button (full width).
	create("BUTTON", "Click to set keybind", 0x0000, margin, 24, width, btnH, idBtnSetKeybind)
	// Keybind label (centered).
	create("STATIC", "Current keybind: Ctrl+Alt+V", 0x0001, margin, 88, width, lblH, idLblKeybind)
	// Volume level label (centered).
	create("STATIC", "Volume: 100%", 0x0001, margin, 124, width, lblH, idLblVolume)
	// Status label (centered).
	create("STATIC", "Ready.", 0x0001, margin, 160, width, lblH, idLblStatus)
	// Quit button (full width).
	create("BUTTON", "Quit", 0x0000, margin, 210, width, btnH, idBtnQuit)
	// "Run on startup" checkbox.
	create("BUTTON", "Run on startup", 0x0003, margin, 280, width, lblH, idChkAutostart)

	a.applyFontToAll()
}

// applyFontToAll sets the custom font on all child controls.
func (a *App) applyFontToAll() {
	const WM_SETFONT = 0x0030
	enumChildProc := windows.NewCallback(func(hwnd windows.HWND, lParam uintptr) uintptr {
		procSendMessage.Call(uintptr(hwnd), WM_SETFONT, uintptr(a.font), 1)
		return 1 // continue
	})
	procEnumChild.Call(uintptr(a.hwnd), enumChildProc, 0)
}

// SetKeybindLabel updates the keybind label text.
func (a *App) SetKeybindLabel(text string) {
	procSetDlgItemText.Call(uintptr(a.hwnd), idLblKeybind, uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(text))))
}

// SetVolumeLabel updates the volume level label text.
func (a *App) SetVolumeLabel(text string) {
	procSetDlgItemText.Call(uintptr(a.hwnd), idLblVolume, uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(text))))
}

// SetStatus updates the status label text.
func (a *App) SetStatus(text string) {
	procSetDlgItemText.Call(uintptr(a.hwnd), idLblStatus, uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(text))))
}

// SetAutostartChecked sets the checked state of the autostart checkbox.
func (a *App) SetAutostartChecked(checked bool) {
	const (
		BM_SETCHECK   = 0x00F1
		BST_CHECKED   = 0x0001
		BST_UNCHECKED = 0x0000
	)
	state := uintptr(BST_UNCHECKED)
	if checked {
		state = BST_CHECKED
	}
	chk, _, _ := procGetDlgItem.Call(uintptr(a.hwnd), idChkAutostart)
	if chk != 0 {
		procSendMessage.Call(chk, BM_SETCHECK, state, 0)
	}
}

// isAutostartChecked reads the checked state of the autostart checkbox.
func (a *App) isAutostartChecked() bool {
	const (
		BM_GETCHECK = 0x00F0
		BST_CHECKED = 0x0001
	)
	chk, _, _ := procGetDlgItem.Call(uintptr(a.hwnd), idChkAutostart)
	if chk == 0 {
		return false
	}
	ret, _, _ := procSendMessage.Call(chk, BM_GETCHECK, 0, 0)
	return ret == BST_CHECKED
}

// wndProc is the window procedure.
func (a *App) wndProc(hwnd windows.HWND, msg uint32, wParam, lParam uintptr) (ret uintptr) {
	// Recover from any panic in the window procedure so a bad message
	// handler doesn't crash the whole process — and LOG it to the file.
	defer func() {
		if r := recover(); r != nil {
			logf("PANIC in wndProc (msg=0x%X): %v", msg, r)
			defRet, _, _ := procDefWindowProc.Call(
				uintptr(hwnd), uintptr(msg), wParam, lParam,
			)
			ret = defRet
		}
	}()

	const (
		WM_COMMAND        = 0x0111
		WM_DESTROY        = 0x0002
		WM_CLOSE          = 0x0010
		WM_HOTKEY         = 0x0312
		WM_CTLCOLORSTATIC = 0x0138
		WM_ERASEBKGND     = 0x0014
		WM_KEYDOWN        = 0x0100
		WM_SYSKEYDOWN     = 0x0104
	)

	switch msg {
	case WM_COMMAND:
		cmd := uint32(wParam & 0xFFFF)
		notify := uint32(wParam >> 16)
		switch cmd {
		case idBtnSetKeybind:
			a.startCapture()
			return 0
		case idBtnQuit:
			a.quit()
			return 0
		case idChkAutostart:
			// BN_CLICKED = 0. The checkbox has already toggled its own
			// state, so read the NEW state and pass it through.
			if notify == 0 && a.onAutostart != nil {
				a.onAutostart(a.isAutostartChecked())
			}
			return 0
		}

	case WM_HOTKEY:
		if a.hotkeys.HandleMessage(msg, wParam, lParam) {
			if a.onToggle != nil {
				a.onToggle()
			}
		}
		return 0

	case WM_KEYDOWN, WM_SYSKEYDOWN:
		// Keybind capture: when in capture mode, the pressed key (with its
		// modifiers) becomes the new keybind. Our window has focus in this
		// state, so no system-wide hook is needed.
		a.mu.Lock()
		capturing := a.capturing
		a.mu.Unlock()
		if capturing {
			a.finishCapture(uint32(wParam))
		}
		return 0

	case WM_CLOSE:
		// Close-to-tray: hide the window instead of quitting.
		a.hideWindow()
		return 0

	case WM_ERASEBKGND:
		a.drawBackground(windows.Handle(wParam))
		return 1 // background erased

	case WM_CTLCOLORSTATIC:
		hdc := windows.Handle(wParam)
		procSetTextColor.Call(uintptr(hdc), colorText)
		procSetBkMode.Call(uintptr(hdc), 1) // TRANSPARENT
		return uintptr(a.brushBG)

	case WM_DESTROY:
		a.removeTrayIcon()
		a.cleanupResources()
		procPostQuitMessage.Call(0)
		return 0

	case wmShowFromSecondInstance:
		// A second instance asked us to show ourselves.
		a.showWindow()
		return 0

	case wmToggleResult:
		// The volume worker finished a toggle; apply its result to the UI.
		// Runs on the message-pump thread: all GUI calls below are legal.
		res := recoverToggleResult(lParam)
		if res == nil {
			return 0
		}
		if res.ErrText != "" {
			a.SetStatus("Error: " + res.ErrText)
		} else {
			a.SetStatus(fmt.Sprintf("Discord volume set to %d%%", res.LevelPct))
			a.SetVolumeLabel(fmt.Sprintf("Volume: %d%%", res.LevelPct))
			a.SetTrayVolume(res.LevelPct)
		}
		return 0

	case wmTrayIcon:
		if lParam == 0x0202 { // WM_LBUTTONUP
			a.showWindow()
		} else if lParam == 0x0205 { // WM_RBUTTONUP
			a.showTrayMenu()
		}
		return 0
	}

	ret, _, _ = procDefWindowProc.Call(
		uintptr(hwnd), uintptr(msg), wParam, lParam,
	)
	return ret
}

// startCapture enters keybind-capture mode.
func (a *App) startCapture() {
	a.mu.Lock()
	a.capturing = true
	a.mu.Unlock()
	// Focus our window so WM_KEYDOWN arrives here (the button the user
	// just clicked would otherwise eat the keystrokes).
	procSetFocus.Call(uintptr(a.hwnd))
	a.SetStatus("Press a key combination...")
}

// finishCapture records the captured keybind.
func (a *App) finishCapture(vk uint32) {
	a.mu.Lock()
	if !a.capturing {
		a.mu.Unlock()
		return
	}
	a.capturing = false
	a.mu.Unlock()

	// Read modifier state (check both generic and left/right variants).
	mods := uint32(0)
	if anyKeyDown(0x11, 0xA2, 0xA3) { // Ctrl
		mods |= hotkey.ModControl
	}
	if anyKeyDown(0x12, 0xA4, 0xA5) { // Alt
		mods |= hotkey.ModAlt
	}
	if anyKeyDown(0x10, 0xA0, 0xA1) { // Shift
		mods |= hotkey.ModShift
	}
	if anyKeyDown(0x5B, 0x5C) { // Win
		mods |= hotkey.ModWin
	}

	// Ignore pure-modifier presses.
	if isModifierKey(vk) {
		a.SetStatus("Press a key combination (include a non-modifier key)...")
		a.mu.Lock()
		a.capturing = true
		a.mu.Unlock()
		return
	}

	k := hotkey.Key{VK: vk, Mods: mods}
	if a.onKeybind != nil {
		a.onKeybind(k)
	}
	a.SetStatus("Keybind set. Press it to toggle Discord volume.")
}

// isModifierKey reports whether the virtual-key code is a modifier key.
func isModifierKey(vk uint32) bool {
	switch vk {
	case 0x10, 0x11, 0x12, 0x5B, 0x5C, // generic Shift, Ctrl, Alt, LWin, RWin
		0xA0, 0xA1, // LShift, RShift
		0xA2, 0xA3, // LCtrl, RCtrl
		0xA4, 0xA5: // LAlt, RAlt
		return true
	}
	return false
}

// quit triggers the quit handler and destroys the window.
func (a *App) quit() {
	if a.onQuit != nil {
		a.onQuit()
	}
	procDestroyWindow.Call(uintptr(a.hwnd))
}

// hideWindow hides the window (close-to-tray behavior).
func (a *App) hideWindow() {
	procShowWindow.Call(uintptr(a.hwnd), 0) // SW_HIDE
}

// showWindow restores the window from the tray.
func (a *App) showWindow() {
	procShowWindow.Call(uintptr(a.hwnd), 9) // SW_RESTORE
	procSetForeground.Call(uintptr(a.hwnd))
}

// cleanupResources destroys theme resources, icons, and the background DIB.
func (a *App) cleanupResources() {
	destroyIcon := user32.NewProc("DestroyIcon")
	if a.brushBG != 0 {
		procDeleteObject.Call(uintptr(a.brushBG))
		a.brushBG = 0
	}
	if a.brushPanel != 0 {
		procDeleteObject.Call(uintptr(a.brushPanel))
		a.brushPanel = 0
	}
	if a.font != 0 {
		procDeleteObject.Call(uintptr(a.font))
		a.font = 0
	}
	if a.fontBold != 0 {
		procDeleteObject.Call(uintptr(a.fontBold))
		a.fontBold = 0
	}
	// Destroy cached tray icons with DestroyIcon (icons are NOT GDI objects).
	for lvl, h := range a.trayIcons {
		if h != 0 {
			destroyIcon.Call(uintptr(h))
		}
		delete(a.trayIcons, lvl)
	}
	if a.iconMain != 0 {
		destroyIcon.Call(uintptr(a.iconMain))
		a.iconMain = 0
	}
	// Free the background DC + bitmap.
	if a.bgMemDC != 0 {
		procDeleteDC.Call(uintptr(a.bgMemDC))
		a.bgMemDC = 0
	}
	if a.bgBitmap != 0 {
		procDeleteObject.Call(uintptr(a.bgBitmap))
		a.bgBitmap = 0
	}
}

// --- Background image (pre-rendered DIB section) ---

// createBackgroundDIB renders the subtle avatar image (from the embedded
// pre-composited pixels) into a Windows-owned DIB section of the exact
// display size, selected into a memory DC. At paint time we only BitBlt —
// no raw Go pointers, no stretching, no per-paint allocation.
//
// It uses the pre-baked pixel data from the background package via
// decodeBackgroundPixels (see background.go).
func (a *App) createBackgroundDIB() {
	// Decode embedded PNG to raw pixels (RGBA), then pre-scale to display
	// size and write BGRA into the DIB section's memory.
	w, h, pixels := decodeBackgroundPixels()
	if w == 0 || h == 0 {
		return
	}

	// Pre-scale to bgDisplaySize x bgDisplaySize (nearest neighbor).
	dw := bgDisplaySize
	dh := bgDisplaySize
	scaled := make([]byte, dw*dh*4)
	for y := 0; y < dh; y++ {
		sy := y * h / dh
		for x := 0; x < dw; x++ {
			sx := x * w / dw
			si := (sy*w + sx) * 4
			di := (y*dw + x) * 4
			// RGBA → BGRA
			scaled[di+0] = pixels[si+2]
			scaled[di+1] = pixels[si+1]
			scaled[di+2] = pixels[si+0]
			scaled[di+3] = 0xFF // opaque
		}
	}

	// BITMAPINFO for a 32bpp top-down DIB.
	var bmi [40]byte
	*(*uint32)(unsafe.Pointer(&bmi[0])) = 40
	*(*int32)(unsafe.Pointer(&bmi[4])) = int32(dw)
	*(*int32)(unsafe.Pointer(&bmi[8])) = -int32(dh) // top-down
	*(*uint16)(unsafe.Pointer(&bmi[12])) = 1
	*(*uint16)(unsafe.Pointer(&bmi[14])) = 32
	*(*uint32)(unsafe.Pointer(&bmi[16])) = 0 // BI_RGB

	var bits uintptr
	hbm, _, _ := procCreateDIBSection.Call(
		0, // hdc (NULL ok for DIB_RGB_COLORS)
		uintptr(unsafe.Pointer(&bmi[0])),
		0, // DIB_RGB_COLORS
		uintptr(unsafe.Pointer(&bits)),
		0, 0,
	)
	if hbm == 0 {
		logf("warning: CreateDIBSection failed")
		return
	}
	a.bgBitmap = windows.Handle(hbm)

	// Copy the scaled pixels into the DIB section's memory.
	//
	// The DIB's memory pointer comes back from CreateDIBSection as a
	// uintptr, so converting it to a pointer here is flagged by vet as a
	// uintptr→unsafe.Pointer round-trip. That check exists because the GC
	// may move... it cannot: Windows owns this memory (non-Go heap), the
	// value never escapes as a stored uintptr, and the conversion happens
	// in a single expression with no allocation or goroutine switch in
	// between, so the conversion is safe as written.
	dst := unsafe.Slice((*byte)(unsafeFromUintptr(bits)), dw*dh*4)
	copy(dst, scaled)

	// Select the bitmap into a memory DC for blitting.
	hdc, _, _ := procCreateCompatibleDC.Call(0)
	if hdc == 0 {
		procDeleteObject.Call(hbm)
		a.bgBitmap = 0
		return
	}
	procSelectObject.Call(hdc, hbm)
	a.bgMemDC = windows.Handle(hdc)
}

// drawBackground fills the window with the dark color and blits the
// pre-rendered avatar image (bottom-right).
func (a *App) drawBackground(hdc windows.Handle) {
	var rc struct{ Left, Top, Right, Bottom int32 }
	procGetClientRect.Call(uintptr(a.hwnd), uintptr(unsafe.Pointer(&rc)))
	cw := int(rc.Right)
	ch := int(rc.Bottom)

	// Fill with the dark background.
	procFillRect.Call(uintptr(hdc), uintptr(unsafe.Pointer(&rc)), uintptr(a.brushBG))

	// Blit the avatar image if available.
	if a.bgMemDC == 0 {
		return
	}
	dstX := cw - bgDisplaySize - 16
	dstY := ch - bgDisplaySize - 16
	if dstX < 0 {
		dstX = 0
	}
	if dstY < 0 {
		dstY = 0
	}
	procBitBlt.Call(
		uintptr(hdc),
		uintptr(dstX), uintptr(dstY),
		bgDisplaySize, bgDisplaySize,
		uintptr(a.bgMemDC),
		0, 0,
		0x00CC0020, // SRCCOPY
	)
}

// --- Tray icon ---

// notifyIconDataSize matches the full x64 NOTIFYICONDATAW structure.
const notifyIconDataSize = 976

const (
	nimAdd    = 0
	nimModify = 1
	nimDelete = 2

	nifMessage = 0x1
	nifIcon    = 0x2
	nifTip     = 0x4
)

// buildNotifyIconData fills a NOTIFYICONDATAW buffer with the common fields.
// Offsets (x64): cbSize=0, hWnd=8, uID=16, uFlags=20, uCallbackMessage=24,
// hIcon=32, szTip=40.
func buildNotifyIconData(hwnd windows.HWND, uID uint32, flags uint32, callbackMsg uint32, hIcon windows.Handle, tip string) []byte {
	nid := make([]byte, notifyIconDataSize)
	*(*uint32)(unsafe.Pointer(&nid[0])) = notifyIconDataSize
	*(*uintptr)(unsafe.Pointer(&nid[8])) = uintptr(hwnd)
	*(*uint32)(unsafe.Pointer(&nid[16])) = uID
	*(*uint32)(unsafe.Pointer(&nid[20])) = flags
	*(*uint32)(unsafe.Pointer(&nid[24])) = callbackMsg
	*(*uintptr)(unsafe.Pointer(&nid[32])) = uintptr(hIcon)
	if tip != "" {
		t := windows.StringToUTF16(tip)
		copy(nid[40:], unsafe.Slice((*byte)(unsafe.Pointer(&t[0])), len(t)*2))
	}
	return nid
}

func (a *App) addTrayIcon() {
	iconHandle := a.trayIcons[trayicon.High]
	nid := buildNotifyIconData(a.hwnd, 1, nifMessage|nifIcon|nifTip, wmTrayIcon, iconHandle, "Discord Volume Toggle")
	procShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&nid[0])))
}

// SetTrayVolume updates the tray icon and tooltip to reflect the current
// volume level (uses only cached icons — no icon creation at runtime).
func (a *App) SetTrayVolume(pct int) {
	var level trayicon.Level
	switch {
	case pct <= 0:
		level = trayicon.Muted
	case pct <= 33:
		level = trayicon.Low
	case pct <= 66:
		level = trayicon.Mid
	default:
		level = trayicon.High
	}
	iconHandle := a.trayIcons[level]
	tip := fmt.Sprintf("Discord Volume Toggle - %d%%", pct)
	nid := buildNotifyIconData(a.hwnd, 1, nifIcon|nifTip, wmTrayIcon, iconHandle, tip)
	procShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&nid[0])))
}

func (a *App) removeTrayIcon() {
	nid := buildNotifyIconData(a.hwnd, 1, 0, 0, 0, "")
	procShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&nid[0])))
}

func (a *App) showTrayMenu() {
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)

	procAppendMenu.Call(menu, 0x0000, 1, uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("Show"))))
	procAppendMenu.Call(menu, 0x0800, 0, 0) // separator
	procAppendMenu.Call(menu, 0x0000, 2, uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("Quit"))))

	var pt struct{ X, Y int32 }
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))

	procSetForeground.Call(uintptr(a.hwnd))

	ret, _, _ := procTrackPopupMenu.Call(menu, 0x0100|0x0002, uintptr(pt.X), uintptr(pt.Y), 0, uintptr(a.hwnd), 0)
	if ret == 1 {
		a.showWindow()
	} else if ret == 2 {
		a.quit()
	}
}

// --- Icon loading (once at startup) ---

func loadMainIcon() windows.Handle {
	return icon.Handle()
}

func loadTrayIcon(level trayicon.Level) windows.Handle {
	data := trayicon.PNG(level)
	h, _, _ := user32.NewProc("CreateIconFromResourceEx").Call(
		uintptr(unsafe.Pointer(&data[0])),
		uintptr(len(data)),
		1, 0x00030000, 0, 0, 0,
	)
	return windows.Handle(h)
}

// --- Win32 helpers ---

func instanceHandle() uintptr {
	h, _, _ := procGetModuleHandle.Call(0)
	return h
}

func loadCursor() uintptr {
	c, _, _ := procLoadCursor.Call(0, 32512) // IDC_ARROW
	return c
}

func anyKeyDown(vks ...uint32) bool {
	getAsyncKeyState := user32.NewProc("GetAsyncKeyState")
	for _, vk := range vks {
		if ret, _, _ := getAsyncKeyState.Call(uintptr(vk)); ret&0x8000 != 0 {
			return true
		}
	}
	return false
}

func createSolidBrush(color uintptr) windows.Handle {
	h, _, _ := procCreateSolidBrush.Call(color)
	return windows.Handle(h)
}

// createFont creates a font with the given size, weight, and face name.
func createFont(size int32, weight int32, face string) windows.Handle {
	h, _, _ := procCreateFontW.Call(
		uintptr(-size), // negative height = character height
		0, 0, 0,
		uintptr(weight),
		0, 0, 0,
		1, // DEFAULT_CHARSET
		0, 0, 0, 0,
		uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(face))),
	)
	return windows.Handle(h)
}
