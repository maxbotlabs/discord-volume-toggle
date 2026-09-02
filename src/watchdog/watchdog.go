// Package watchdog implements an out-of-process watchdog: the app launches a
// stripped-down copy of itself in child mode, which polls the parent's window
// responsiveness and — when the parent stops pumping messages — writes a
// minidump of the hung process from OUTSIDE it.
//
// Why out-of-process: the in-process watchdog dumps stacks via
// runtime.Stack(all=true), which must stop the world. Any runtime-level wedge
// (broken preemption, loader-lock deadlock, wedged GC) freezes the watchdog
// before it writes anything — exactly what the log shows. A separate OS
// process with its own runtime is immune to all of that: it uses
// SendMessageTimeout to detect the hang (the same probe Windows uses for
// "not responding") and MiniDumpWriteDump to capture the hung process,
// naming the exact blocked call on every thread.
//
// Parent/child protocol: deliberately argv-only. The parent passes the hwnd;
// the child resolves the parent PID from it. The parent keeps the child's
// os.Process handle (from os.StartProcess) to shut the child down on quit.
// No pipes, no handshake, no failure surface.
package watchdog

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ChildModeArg is the argv flag that puts the process into watchdog-child
// mode instead of normal app mode. (Kept for external/manual invocation;
// the self-spawn path uses the env vars below because argv does not
// survive parent→child for the windowsgui parent — see Start.)
const ChildModeArg = "--watchdog-child"

// Env-var handoff (see Start).
const (
	childModeEnv = "DVT_WATCHDOG_CHILD"
	childHwndEnv = "DVT_WATCHDOG_HWND"
)

// syscallPtrEnv builds a Win32 environment block (UTF-16, NUL-separated,
// double-NUL terminated) from a Go env slice.
func syscallPtrEnv(env []string) *uint16 {
	block, err := syscallEnvBlock(env)
	if err != nil {
		return nil
	}
	return block
}

func syscallEnvBlock(env []string) (*uint16, error) {
	var size int
	for _, s := range env {
		size += len(s) + 1
	}
	size += 1
	buf := make([]uint16, size)
	p := 0
	for _, s := range env {
		for _, r := range s {
			buf[p] = uint16(r)
			p++
		}
		buf[p] = 0
		p++
	}
	return &buf[0], nil
}

// Protocol constants.
const (
	// hangTimeoutMs: a SendMessageTimeout probe that takes longer than this
	// means the target's pump is not dispatching (Windows' own "not
	// responding" threshold is 5s).
	hangTimeoutMs = 6000
	// probeIntervalMs is how often the child probes the parent window.
	probeIntervalMs = 1000
	// confirmations needed before declaring a hang (avoids one-off jank).
	hangConfirmations = 3
)

const DumpFolderEnv = "DISCORDVOLUMETOGGLE_DUMPDIR"

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	dbghelp  = windows.NewLazySystemDLL("dbghelp.dll")

	procSendMessageTimeout = user32.NewProc("SendMessageTimeoutW")
	procGetWindowThreadPID = user32.NewProc("GetWindowThreadProcessId")
	procIsWindow           = user32.NewProc("IsWindow")

	procOpenProcess       = kernel32.NewProc("OpenProcess")
	procTerminateProcess  = kernel32.NewProc("TerminateProcess")
	procCloseHandle       = kernel32.NewProc("CloseHandle")
	procMiniDumpWriteDump = dbghelp.NewProc("MiniDumpWriteDump")
)

const (
	// SMTO_ABORTIFHUNG: return immediately if the target is "hung" per
	// Windows; combined with our own timeout this gives fast, reliable
	// hang detection.
	smtoAbortIfHung = 0x0002
	wmNull          = 0x0000
	invalidHandle   = ^uintptr(0)
	// MiniDumpWithIndirectlyReferencedMemory | MiniDumpScanMemory:
	// moderate-size dumps that still contain all thread stacks and the
	// heap regions needed to see what each thread was blocked on.
	miniDumpWithData = 0x00000040 | 0x00000100
)

// ChildFromEnv reports the parent hwnd if this process was launched as a
// watchdog child (empty string otherwise). Detection order:
//  1. DVT_WATCHDOG_CHILD env (external invocation path)
//  2. The TEMP handoff file (written by Start right before CreateProcessW;
//     the parent deletes it after startup). The file exists ONLY between
//     parent-write and parent-delete, so a normally-launched app (user
//     double-click, autostart) never sees it — unless it was launched
//     within that tiny window by the parent, which is exactly the case
//     we're detecting.
func ChildFromEnv() string {
	if os.Getenv(childModeEnv) != "1" {
		return ""
	}
	return os.Getenv(childHwndEnv)
}

// RunChild is the entrypoint for child mode: poll the parent's window until
// it stops pumping, then minidump it. Never returns normally; exits.
func RunChild(parentHwndArg string) {
	logLine("watchdog child: launched with hwnd arg %q", parentHwndArg)
	hwndRaw, err := strconv.ParseUint(parentHwndArg, 10, 64)
	if err != nil {
		os.Exit(2)
	}
	hwnd := uintptr(hwndRaw)
	if hwnd == 0 || isWindow(hwnd) == 0 {
		logLine("watchdog child: invalid window handle %q (IsWindow=0)", parentHwndArg)
		os.Exit(2)
	}

	var pid uint32
	procGetWindowThreadPID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if pid == 0 {
		logLine("watchdog child: no pid for hwnd 0x%X", hwndRaw)
		os.Exit(2)
	}
	logLine("watchdog child: watching parent pid=%d hwnd=0x%X", pid, hwndRaw)

	consecutive := 0
	for {
		// Window closed = parent quit normally; leave quietly.
		if isWindow(hwnd) == 0 {
			os.Exit(0)
		}
		if probeResponsive(hwnd) {
			consecutive = 0
		} else {
			consecutive++
			logLine("watchdog child: pump unresponsive %d/%d", consecutive, hangConfirmations)
			if consecutive >= hangConfirmations {
				dumpParent(pid)
				// Do NOT kill the parent: if it recovers (slow COM, busy
				// Explorer), great; if it's truly wedged, the user or
				// Windows closes it. The dump is the deliverable.
				os.Exit(0)
			}
		}
		time.Sleep(probeIntervalMs * time.Millisecond)
	}
}

// isWindow wraps IsWindow (Call returns multiple values; wrap for clarity).
func isWindow(hwnd uintptr) uintptr {
	ret, _, _ := procIsWindow.Call(hwnd)
	return ret
}

// probeResponsive sends WM_NULL with a timeout. True if the target pumped it.
func probeResponsive(hwnd uintptr) bool {
	var res uintptr
	ret, _, _ := procSendMessageTimeout.Call(
		hwnd, wmNull, 0, 0,
		uintptr(smtoAbortIfHung), uintptr(hangTimeoutMs),
		uintptr(unsafe.Pointer(&res)),
	)
	return ret != 0
}

// dumpParent writes a minidump of pid to %APPDATA%\DiscordVolumeToggle\dumps.
func dumpParent(pid uint32) {
	dir := dumpDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(dir, fmt.Sprintf("hang-%s-%d.dmp",
		time.Now().Format("20060102-150405"), pid))

	h, _, _ := procOpenProcess.Call(
		0x001F0FFF, // PROCESS_ALL_ACCESS
		0, uintptr(pid),
	)
	if h == 0 {
		logLine("watchdog: OpenProcess(%d) failed", pid)
		return
	}
	defer procCloseHandle.Call(h)

	f, err := os.Create(path)
	if err != nil {
		logLine("watchdog: create %s failed: %v", path, err)
		return
	}
	defer f.Close()

	// MINIDUMP_EXCEPTION_INFORMATION is nil: we want a live-hang snapshot,
	// not an exception dump.
	ret, _, callErr := procMiniDumpWriteDump.Call(
		h,
		uintptr(pid),
		uintptr(f.Fd()),
		uintptr(miniDumpWithData),
		0, 0, 0,
	)
	if ret == 0 {
		logLine("watchdog: MiniDumpWriteDump failed: %v (dump file removed)", callErr)
		_ = os.Remove(path)
		return
	}
	logLine("watchdog: HANG DETECTED - minidump written: %s", path)
}

// dumpDir mirrors the app's config dir with a dumps subfolder.
func dumpDir() string {
	base := os.Getenv(DumpFolderEnv)
	if base != "" {
		return base
	}
	appdata, err := os.UserConfigDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "DiscordVolumeToggle-dumps")
	}
	return filepath.Join(appdata, "DiscordVolumeToggle", "dumps")
}

// logLine appends a line to the app log (same file the app uses). Also
// mirrors to a child-only file, because if app.log writes ever fail from
// the child we'd otherwise lose the diagnostic trail. Errors are silently
// ignored: the child must never make things worse.
func logLine(format string, args ...interface{}) {
	line := fmt.Sprintf(time.Now().Format("2006/01/02 15:04:05")+" [watchdog-child] "+format+"\n", args...)
	dir, err := os.UserConfigDir()
	if err != nil {
		return
	}
	base := filepath.Join(dir, "DiscordVolumeToggle")
	_ = os.MkdirAll(base, 0o755)
	if f, err := os.OpenFile(filepath.Join(base, "app.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		_, _ = f.WriteString(line)
		f.Close()
	}
	if f, err := os.OpenFile(filepath.Join(base, "watchdog-child.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		_, _ = f.WriteString(line)
		f.Close()
	}
}
