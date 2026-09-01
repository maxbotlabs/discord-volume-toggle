# The Bug — Full Evidence Trail

> Written for a fresh set of eyes. Everything we know, everything we tried,
> everything that didn't work, and the open questions. If you can spot what
> we missed, that's the whole point of this file.

## TL;DR

The app intermittently **hangs** ("stopped interacting with Windows", Windows
Event Log **Application Hang, Event ID 1002**) after running for anywhere from
seconds to ~30 minutes. Windows then kills it. There is **no Go panic, no
crash dump, and — critically — no watchdog stack dump**, even after we added a
watchdog that should fire within ~4 seconds of a stall.

The hang happens during **pure idle** (no hotkey press, no toggle in flight)
as often as during active use.

## The app

A native Windows desktop app in pure Go (no cgo), cross-compiled from Linux:

```
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build \
  -ldflags "-H windowsgui" -o DiscordVolumeToggle.exe ./cmd/discord-volume-toggle
```

- Global hotkey (`RegisterHotKey`) cycles Discord's per-app output volume
  (100% → 50% → 25% → 0% → 100%) via WASAPI (`github.com/moutend/go-wca`).
- Raw Win32 GUI (no framework): window, controls, system tray, message pump.
- Logs to `%APPDATA%\DiscordVolumeToggle\app.log`.

## Environment

- Windows 11 (user's daily driver)
- Go 1.23, cross-compiled from Linux
- Other audio sessions running at the time: `LEDKeeper2.exe` (RGB software),
  `steam.exe`, `Discord.exe`

## Evidence timeline

### Early builds (18:29–19:00) — feature bugs, not the hang

- Light theme, a low-level keyboard hook (`WH_KEYBOARD_LL`) for keybind
  capture, per-keystroke hook posting.
- Ran ~30 min OK, then restarts due to feature bugs (not the hang).

### 19:07–19:12 — launch crash (fixed)

- `RegisterClassEx` failed on launch.
- Root cause: `WNDCLASSEX.hbrBackground` was set to a raw `COLORREF` instead
  of a brush handle. Fixed by creating a real `HBRUSH`.

### 19:16–19:34 — the hang appears

- Silent hang/crash after minutes. Last log line is always either a completed
  toggle (`toggle complete (Discord at N%)`) or a heartbeat during idle.
- Event log: **Application Hang 1002** — "stopped interacting with Windows and
  was closed."

### 20:05 — leak theory tested, disproven

- Added a heartbeat that logs process handle/GDI/USER object counts every 30s.
- Sample: `handles=291 gdi=30 user=23` — healthy, flat. **No leak.**

### 20:18 — precise hang timing captured

```
20:18:42 toggle complete (Discord at 100%)
20:18:52 [gui] alive (idle): handles=291 gdi=30 user=23
20:19:04  <-- Windows Event 1002 (hang detected + closed)
```

- The loop was alive at 20:18:52 (heartbeat), hung by ~20:18:54–58, detected
  at 20:19:04. **12 seconds, no input in flight, no toggle pending.**

### 20:35 — latest run, still no dump

```
20:35:47 toggle complete (Discord at 25%)
20:35:55 [gui] alive (idle): handles=300 gdi=30 user=24
20:36:25 [gui] alive (idle): handles=300 gdi=30 user=25
```

- Handles stable at 300, GDI 30, USER 24→25. Still healthy.
- App died again after this with **no watchdog dump**.

## What we tried (and the result)

### 1. Removed the low-level keyboard hook

The `WH_KEYBOARD_LL` hook was the first suspect: its callback chains into
other processes' hooks (RGB software, overlays) on every system keystroke, and
a slow hook anywhere in the chain blocks our message loop until Windows
declares us "not responding."

**Result:** Replaced with `WM_KEYDOWN` capture (our window has focus during
capture). The hang persisted. **Not the cause.**

### 2. Fixed a per-toggle icon leak

Tray icons were being created on every toggle instead of cached. Fixed (icons
now cached once at startup).

**Result:** No change to the hang. (And the leak theory was later disproven by
the handle counts anyway.)

### 3. Watchdog v1 (15s threshold)

A watchdog goroutine ticks every 5s, checks a counter the message pump
increments every iteration, and dumps all goroutine stacks if the loop stalls
for ~15s.

**Result:** Never fired. The process died at ~12s — **Windows beat the
watchdog to the kill.**

### 4. Watchdog v2 (4s threshold + direct write)

Tightened to tick every 2s and dump at 4s of stall. Also changed the dump to
write **directly to the log file**, bypassing the `log` package's internal
mutex — so a main thread stuck while holding the logger lock can't block the
dump.

**Result:** Still no dump on the latest death (20:36+). This is the key
unresolved observation — see below.

## The key unresolved observation

The fast watchdog (fires at ~4–6s of stall) **still produced no dump**. That
narrows it to one of two things:

1. **The entire Go process froze** — main thread *and* the watchdog goroutine
   *and* the timer goroutines all stopped executing. A hang detected by
   Windows, but no Go code running to dump anything. This points at a
   runtime-level freeze, not a single blocked syscall.

2. **The process was killed within ~4–6s of the stall** by something external
   (Windows auto-terminate, or the user clicking "Close the program" on the
   ghost dialog before the watchdog could fire).

If it's (1), candidates worth investigating:

- **Loader lock / DLL_THREAD_DETACH deadlock.** A thread holding the loader
  lock while another blocks on it can freeze the whole process. WASAPI/COM
  (`go-ole`, `go-wca`) does a lot of DLL loading and COM apartment work.
- **COM STA re-entrancy.** `ole.CoInitializeEx(0, COINIT_APARTMENTTHREADED)`
  is called once on the main thread. If a COM call re-enters the apartment in
  a way that deadlocks, the whole STA (and thus the message pump) freezes.
- **A blocked file write.** Both the heartbeat (main thread) and the watchdog
  dump write to the same `app.log`. If a write blocks (antivirus scanning the
  file, a filesystem hiccup), the main thread freezes on the heartbeat write,
  and the watchdog's dump write blocks on the same file. This would produce
  *exactly* the observed symptom: hang detected, no dump, no heartbeat.

## How to reproduce

1. Build (command above) and run `DiscordVolumeToggle.exe`.
2. Spam the hotkey hard for a bit, then leave it idle.
3. The hang is intermittent — sometimes seconds, sometimes ~30 min.
4. On death, check `%APPDATA%\DiscordVolumeToggle\app.log` and the Windows
   Event Log (Application, Event ID 1002).

## Where the log lives

`%APPDATA%\DiscordVolumeToggle\app.log` (append mode). Toggle activity, config
saves, GUI panics, heartbeats, and (in theory) watchdog dumps all land here.

## Build

```bash
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build \
  -ldflags "-H windowsgui" -o DiscordVolumeToggle.exe ./cmd/discord-volume-toggle
```

## Layout

```
discord-volume-toggle/
├── cmd/discord-volume-toggle/   # main entrypoint (wiring, logging, single-instance)
├── src/
│   ├── autostart/               # registry Run key (run on startup)
│   ├── background/              # embedded pre-composited background PNG
│   ├── config/                  # JSON config (keybind, levels, process name)
│   ├── gui/                     # Win32 window, controls, message pump, watchdog, tray
│   ├── hotkey/                  # RegisterHotKey manager
│   ├── icon/                    # embedded app icon
│   ├── trayicon/                # embedded volume-level tray icons
│   └── volume/                  # WASAPI per-app volume (go-wca)
├── assets/                      # icon/tray/background generators + sources
└── docs/                        # design notes
```

The message pump and watchdog live in `src/gui/gui.go` (`App.Run`). The
WASAPI volume logic is in `src/volume/volume.go`. The toggle handler wiring is
in `cmd/discord-volume-toggle/main.go`.
