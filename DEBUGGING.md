# The Bug — Full Evidence Trail

> Written for a fresh set of eyes. Everything we know, everything we tried,
> everything that didn't work, and the open questions. If you can spot what
> we missed, that's the whole point of this file.

## TL;DR — RESOLVED

The app intermittently **hangs** ("stopped interacting with Windows", Windows
Event Log **Application Hang, Event ID 1002**) after running for anywhere from
seconds to ~30 minutes.

**Root cause (proven 2026-09-01):** the main/pump goroutine was never pinned
to its OS thread. When the Go scheduler migrates it, Win32 sent-messages go
to the *window-owning thread's* queue, which nobody drains anymore; the pump
keeps looping on the wrong thread (heartbeats continue!) while the window
starves → "not responding". **Fix: `runtime.LockOSThread()` at the top of
`main()`** (cmd/discord-volume-toggle/main.go) — applied. Full evidence trail
below.

This file remains as the debugging story: what we knew, what we tried, the
wrong turns (with why they were wrong), and how the root cause was finally
caught — for the next person with a similar ghost.

*(The hang happened during **pure idle** as often as during active use —
explained by the root cause below: goroutine migration is triggered by
scheduler churn, which happens during GC, timers, and toggles alike.)*

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

**Result:** Still no dump on the latest death (20:36+). Explained below —
"Why the watchdog never dumped".

## ROOT CAUSE FOUND (2026-09-01 20:14): goroutine migration off the window thread

The goroutine-dump goroutine (60s snapshots) caught the hung process with
**every goroutine in a perfectly healthy state**:

```
goroutine 1  (pump):   [syscall]  — idle in MsgWaitForMultipleObjects
goroutine 21 (worker): [chan receive, locked to thread] — idle
goroutine 23 (watchdog): [select] — idle
```

...while `app.log` heartbeats **continued on schedule** (20:13:02, 20:13:32,
20:14:02) and Windows still reported the window "not responding."

That combination is only possible one way: **the pump goroutine had been
migrated by the Go scheduler to a different OS thread than the one that owns
the window.** Win32 delivers *sent* messages to the message queue of the
thread that owns the target window; `PeekMessage`/`MsgWaitForMultipleObjects`
drain the *current* thread's queue only. Once migrated:

- the pump keeps looping on the wrong thread (heartbeats continue!),
- the real queue — on the original window thread — accumulates everything
  (all our posted messages, Explorer's `Shell_NotifyIcon` traffic, the
  watchdog's `WM_NULL` probes),
- nothing on that queue is ever dispatched → the window never answers sent
  messages → Windows flags it "not responding" → ghost window → Event 1002.

Why it looks random: goroutine migration happens at scheduler whim (GC, block
on channel, etc.) — after enough runtime churn (toggles, GC cycles, idle
timers), one day the pump lands on the wrong thread. Matches every observed
pattern: idle hangs, post-toggle hangs, seconds-to-30-minutes variance.

### The fix

`runtime.LockOSThread()` at the top of `main()` — before the window is
created — pins the main goroutine (and its descendants' thread affinity
inheritance) so the pump runs on the window-owning thread forever. This is
standard practice for Go Win32 GUI apps; the #71242 reproducer had it in
`init()` too. The COM worker goroutine already called `LockOSThread` for its
own apartment — the main goroutine just never did.

### Corroborating detail

During the hung window, the window's owning OS thread (29736) was alive in a
Win32 wait, the process was full of healthy parked threads, and **nothing in
the Go runtime was wedged** (dump goroutine kept firing every 60s). The
earlier "STW that cannot complete" reading of the minidump was wrong — those
stacks were a *healthy* idle runtime sampled at the same moment the window
queue was starving.

## Live-captured evidence (2026-09-01 evening)

**The hang was finally captured twice by the out-of-process watchdog** (and
once while the app was idle — no toggles in flight):

- `dumps/hang-20260901-180350-23376.dmp` (after toggle spam)
- `dumps/hang-20260901-181608-44840.dmp` (**pure idle** — the clearest one)

The second dump was captured mid-freeze, and the app **recovered by itself**
~15–20s later (heartbeats resumed at 18:16:21). So this is a recurring
*transient* full-runtime freeze, not an instant death.

### What the frozen-state minidump shows (symbolized)

Raw stacks were mapped to Go symbols (`go tool nm` + minidump thread-stack
scan; see the note on stack orientation below):

| Thread | State at freeze |
|---|---|
| main (pump) | returning from `MsgWaitForMultipleObjects`, parked in `runtime.exitsyscall` — an M **held at the syscall boundary while the world is stopped** |
| COM/worker thread | `runtime.semasleep` (parked on a runtime semaphore) under `combase.dll` — waiting on a runtime lock/chan |
| timer goroutine | `runtime.timers.run` → `runtime.chansend` — **a Ticker trying to deliver a tick to a receiver that has stopped receiving** |
| runtime thread | `runtime.notesleep` — parked M |
| netpoll | healthy wait |

The `timers.run → chansend` detail is the tell: the in-process watchdog's
2s ticker was *full* — its receiver (the watchdog goroutine) had stopped
receiving. Combined with the main M parked in `exitsyscall` while
`SendMessageTimeout(WM_NULL)` from outside timed out: **a stop-the-world was
in progress and could not complete.** The pump was not blocked in a handler;
the *scheduler itself* was wedged.

### Stack-orientation note (for future dump analysis)

MINIDUMP thread stacks here start at the *low* address (current RSP region):
**newest frames are at the START of the captured region**, oldest at the end.
`walkdump*.py`-style scans that read from offset 0 first are reading the
newest frames. Map dump addresses to Go symbols via
`exe_base_in_dump + 0x140000000` → `go tool nm` symbol table.

### Why the fingerprint was absent (resolved, again)

The in-process watchdog's `emergencyDump` *did* try to run (its ticker chan
backed up), but its `runtime.Stack(all=true)` is itself the STW that wedged —
so its fingerprint write never landed either. Everything above the freeze is
consistent with **a runtime/scheduler wedge, not an app-level deadlock**: the
pump goroutine was fine; the *runtime* couldn't restart the world.

### Current leading theory

A goroutine that calls into COM via `go-ole`'s `stdcall` (the WASAPI toggle
path, or COM's internal apartment threads) ends up holding/awaiting runtime
scheduler state across a blocking kernel wait. When a GC cycle (STW) then
tries to stop the world, the scheduler deadlocks: STW waits for Ms, an M is
stuck at `exitsyscall`, another sits in `semasleep` on the same lock. The
world stops for 15–20s — or until Windows kills the app.

`gcstoptheworld=2` (every GC fully stop-the-world) makes GC *stoppings* more
frequent but shorter; the wedge persists regardless — the freeze reproduces
with the flag verified-in-binary.

### Next-hang witness (opt-in)

- **`DVT_DEBUG=1`** enables the 60s goroutine snapshot to
  `%APPDATA%\DiscordVolumeToggle\goroutines.log` (single-slot, truncated each
  write — never grows) plus the 30s `alive (idle):` heartbeat. **Both are off
  by default** — shipped builds are quiet. This witness is what cracked the
  2026-09-01 hang; see "ROOT CAUSE FOUND".
- WER LocalDumps (`capture-crash-dump.reg`, merge once) captures the
  process when Windows terminates it after the hang — from *outside* the
  wedged runtime, no env var needed.

## How to collect the next hang

1. Merge `capture-crash-dump.reg` (once). Dumps land in `C:\dumps`.
2. If you want goroutine-level evidence too, run with `DVT_DEBUG=1` set.
   When it hangs:
   - `%APPDATA%\DiscordVolumeToggle\goroutines.log` — last 60s snapshot
     (Go goroutine states),
   - `C:\dumps\DiscordVolumeToggle.exe.*.dmp` — the OS-level dump.

## Likely root cause: GC/preemption wedge at mark termination (corrected)

**The #71242 wedge is the right *family*, but the original write-up got a key
mechanism wrong.** Verified against the Go 1.23 runtime source (go.mod pins
`go 1.23`; citations below are `runtime/` line areas in go1.23.0):

- **`asyncpreemptoff=1` disables *all* `SuspendThread`-based preemption, not
  just async preemption.** Every caller of `preemptM` — the async-preempt path
  in `preempt.go` (`suspendG`, gated on `debug.asyncpreemptoff == 0`) *and*
  `preemptone` in `proc.go` — checks that flag before touching
  `SuspendThread`/`ResumeThread` (`os_windows.go:1235`). The only other
  runtime user of `SuspendThread` is the CPU profiler, which is off here.
  **With v0.1.1's flag applied, the hooked-DLL wedge from #71242 should be
  impossible.**

That yields two possibilities, both actionable:

1. **The `-X` flag never actually landed in the binary.** This is the likely
   one. The original build line was

   ```
   -ldflags "-H windowsgui -X=runtime.godebugDefault=asyncpreemptoff=1"
   ```

   which is valid, but easy to fat-finger or drop when cross-compiling from
   Linux — and nothing in the app *verifies* its own GODEBUG state. (This
   repo now builds with
   `-X=runtime.godebugDefault=asyncpreemptoff=1,gcstoptheworld=2` and the
   string is verified present in the binary; see "Test it fast" for a
   self-check.)

2. **The wedge is a different mechanism** with the same STW shape — e.g. the
   loader-lock/COM apartment suspects listed below, or a DLL hooking
   `GetThreadContext` (which `preemptM` *also* calls, but only on the async
   path, so it would be equally dead with the flag applied).

### Why the watchdog never dumped — resolved

`emergencyDump` calls `runtime.Stack(stacks, true)`. **With `all=true` that
call does a stop-the-world first** (`runtime/mprof.go`, `func Stack`: `stw =
stopTheWorld(stwAllGoroutinesStack)`). If one thread is wedged in a broken
preemption (or a loader-lock deadlock, or a blocked kernel call that STW must
wait for), **STW never completes and the watchdog freezes inside its own dump
call** — before writing a single byte. The watchdog was never "beaten to the
kill"; it died in the same wedge it was trying to report. That closes the old
"key unresolved observation" option (1) with a concrete mechanism.

Mitigation now in the code: `emergencyDump` writes a **pre-staged
fingerprint line first** ("if no stack dump follows this line, the watchdog
froze inside runtime.Stack(all=true)"), then attempts the full dump. Next
death tells us which happened:

- fingerprint + stacks → loop stalled on an ordinary blocking call (and the
  stacks name it),
- fingerprint, no stacks → runtime-level STW wedge (consistent with
  possibility 1 above: the preemption fix didn't land),
- nothing at all → the process died before the 4s watchdog threshold.

### Why hangs cluster after toggles and during idle

`forcegcperiod` is 2 minutes (`proc.go`:6017), and the GC trigger is a heap
allocation target. Every toggle allocates a burst (~a dozen COM wrapper
objects plus `fmt.Sprintf`s in `volume.go`), so a GC cycle tends to start
right after a toggle; during idle, GC fires on the 2-minute forcegc timer.
Either way, GC's *mark termination* STW and the `suspendG` preemption of
running goroutines land at the worst moment if the wedge is live. This
unifies the "hangs during pure idle" and "died N seconds after a toggle"
patterns the old timeline shows.

## Test it fast

1. **Self-check the GODEBUG flag** (closes possibility 1):
   `Select-String -Path DiscordVolumeToggle.exe -Pattern "asyncpreemptoff=1"`
   — if that string isn't in the binary, the -X never landed.
2. **GOGC=off run**: launch with `GOGC=off`. If the app suddenly runs for
   hours, the hang is GC-preemption-linked — near-conclusive for this theory.
3. **Check injected DLLs**:
   `tasklist /m /fi "imagename eq DiscordVolumeToggle.exe"` (or Process
   Explorer) — look for `gameoverlayrenderer64.dll` (Steam) or
   LEDKeeper/aura DLLs in the process.
4. **Merge `capture-crash-dump.reg`** — WER LocalDumps captures hangs from
   *outside* the frozen runtime (unlike the in-process watchdog). If the
   theory's right, the dump shows one thread in
   `ResumeThread`/`GetThreadContext` (Go runtime, in `preemptM`) and the rest
   blocked in `NtWaitForAlertByThreadId`-style STW waits.
5. **Kill-switch trial**: exit Steam + LEDKeeper2, reproduce with heavy
   toggling.
6. **Structural check (already fixed in code)**: the toggle now runs on a
   worker goroutine, so a slow toggle can no longer stall the pump — if
   hangs persist as *GUI hangs* rather than process death, they're now
   observable via the watchdog fingerprint instead of being masked by an
   unresponsive pump.

## The fixes (applied)

1. **Build flag**: `-ldflags "-H windowsgui
   -X=runtime.godebugDefault=asyncpreemptoff=1,gcstoptheworld=2"`.
   `gcstoptheworld=2` makes every GC a full stop-the-world cycle
   (`extern.go`:88–91; mode upgrade in `mgc.go`:643–651) — no concurrent
   background mark workers, no `suspendG` preemption of running threads, i.e.
   the exact SuspendThread wedge path is removed. STW pauses get slightly
   longer — irrelevant for a tray utility. (`parsegodebug` splits the
   comma-joined list left-to-right, and env GODEBUG overrides -X defaults —
   `runtime1.go` — so a stray user GODEBUG can't re-enable the wedge path.)
2. **Structural**: the toggle used to run WASAPI/RPC synchronously on the UI
   thread (WM_HOTKEY handler → `SetAppVolume`). It now runs on a dedicated
   worker goroutine with its own MTA COM apartment
   (`cmd/discord-volume-toggle/main.go`), which posts completion back to the
   pump via a custom `WM_TOGGLE_RESULT` message
   (`src/gui/gui.go`). The pump keeps ticking during slow audio-service
   calls or busy-Explorer `Shell_NotifyIcon` calls, and the watchdog stays
   meaningful (it measures pump health, not COM latency).
   One toggle in flight at a time; the level index advances at enqueue time
   so rapid presses still cycle levels.
3. **Watchdog ordering**: fingerprint-before-dump (see above), so the next
   event is self-explaining in the log.

## The key unresolved observation (superseded)

Answered: see "Why the watchdog never dumped". The remaining open question is
only *which* of the two wedge mechanisms applies (flag not applied vs.
different mechanism) — test 1 above settles it cheaply, and the
fingerprint-first watchdog makes the *next* event self-explaining either way.

### If it's neither: candidates that remain

- **Loader lock / DLL_THREAD_DETACH deadlock.** A thread holding the loader
  lock while another blocks on it can freeze the whole process. WASAPI/COM
  (`go-ole`, `go-wca`) does a lot of DLL loading and COM apartment work.
  (Note: COM init now happens on two threads — the MTA worker and the UI
  thread's STA — so apartment teardown matters.)
- **COM STA re-entrancy.** `ole.CoInitializeEx(0, COINIT_APARTMENTTHREADED)`
  is still called once on the main thread. If a COM call re-enters the
  apartment in a way that deadlocks, the whole STA (and thus the message
  pump) freezes.
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
  -ldflags "-H windowsgui -X=runtime.godebugDefault=asyncpreemptoff=1,gcstoptheworld=2" \
  -o DiscordVolumeToggle.exe ./cmd/discord-volume-toggle
```

After building, verify the GODEBUG default actually landed:

```powershell
Select-String -Path DiscordVolumeToggle.exe -Pattern "asyncpreemptoff=1,gcstoptheworld=2" -Quiet
# expect: True
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
