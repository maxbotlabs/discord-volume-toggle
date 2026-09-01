# Discord Volume Toggle — Design Notes

## Goal

Native Windows 11 app. Global hotkey toggles Discord output volume 50% ↔ 100%.

## Architecture

- **Language:** Go (single static `.exe`, no runtime)
- **GUI:** Win32 native via `syscall` + `golang.org/x/sys/windows`
- **Per-app volume:** Windows Core Audio API (IMMDeviceEnumerator → IAudioSessionManager2 → IAudioSessionControl2 → ISimpleAudioVolume)
- **Global hotkey:** `RegisterHotKey` + message loop
- **Tray:** `Shell_NotifyIcon`

## Key technical decisions

1. **Core Audio via COM** — the fiddly part. Need to enumerate audio sessions,
   find the one whose process is `Discord.exe`, and set its `ISimpleAudioVolume`
   master volume. This is the well-trodden but verbose piece.

2. **No external GUI toolkit** — avoids shipping a huge runtime. Native Win32
   keeps the `.exe` small and dependency-free. Trade-off: more manual UI code.

3. **Keybind capture** — a "click to set keybind" button that enters a capture
   mode, listens for the next key/combo, and stores it. Persist to a config
   file next to the exe (or `%APPDATA%`).

## Alpha scope (this pass)

- Functional: hotkey toggle works, keybind settable, tray icon, quit.
- Basic UI (functional, not pretty).

## Post-alpha (styling pass)

- Dark theme, proper spacing, hover states, nice keybind display.
- Maybe a volume level indicator (shows current 50%/100% state).

## Config persistence

- Store keybind + toggle levels in a JSON file at `%APPDATA%\DiscordVolumeToggle\config.json`.
