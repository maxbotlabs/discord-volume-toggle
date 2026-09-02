# Discord Volume Toggle

A native Windows desktop app (pure Go, no cgo) that binds a global hotkey to
cycle Discord's output volume (100% → 50% → 25% → 0%).

## Status

**Beta — the intermittent hang is fixed.** The app would intermittently stop
responding after minutes of use (Windows "Application Hang", Event 1002).
Root cause: the main/pump goroutine was never pinned to its OS thread, so the
Go scheduler could migrate it and leave the window's message queue undrained.
Fixed with `runtime.LockOSThread()` at the top of `main()`, plus moving
WASAPI/COM toggle work off the UI thread onto a dedicated worker goroutine.

See [DEBUGGING.md](DEBUGGING.md) for the full evidence trail — it's kept as
the debugging story (what was tried, the wrong turns, and how the root cause
was finally caught) for the next person with a similar ghost.

## Features

- [x] Global hotkey, user-configurable ("click to set keybind" — press any
      key/combo while the app window is focused)
- [x] Multi-step volume cycle: 100% → 50% → 25% → 0% → 100%
- [x] Keybind persists across restarts (`config.json` in `%APPDATA%\DiscordVolumeToggle`)
- [x] System tray: close-to-tray, dynamic tray icon showing current volume
      level (muted/low/mid/high speaker glyph) + tooltip percentage
- [x] Run-on-startup checkbox (per-user registry Run key)
- [x] Dark theme, custom icon, subtle avatar background (pre-rendered DIB)
- [x] Single-instance guard (second launch shows the first instance's window)
- [x] Stall watchdog: dumps goroutine stacks to the log if the message loop
      freezes (opt-in via `DVT_DEBUG=1`)

## Tech

- **Language:** Go 1.23, no cgo
- **Cross-compiled from Linux:** `GOOS=windows GOARCH=amd64`
- **Per-app volume:** Windows Core Audio API (WASAPI via github.com/moutend/go-wca)
- **Global hotkey:** `RegisterHotKey` (Win32)
- **GUI:** raw Win32 via `golang.org/x/sys/windows` (window, controls, tray,
  message pump) — no framework

## Build

```bash
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build \
  -ldflags "-H windowsgui -X=runtime.godebugDefault=asyncpreemptoff=1,gcstoptheworld=2" \
  -o DiscordVolumeToggle.exe ./cmd/discord-volume-toggle
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
├── docs/                        # design notes
└── DEBUGGING.md                 # the hang: resolved root cause + evidence trail
```

## Log

The app logs to `%APPDATA%\DiscordVolumeToggle\app.log` (append). Toggle
activity, config saves, GUI panics, and watchdog dumps all land there.