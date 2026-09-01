// Command discord-volume-toggle is a native Windows app that binds a global
// hotkey to toggle Discord's output volume between two levels.
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"

	"golang.org/x/sys/windows"

	"discord-volume-toggle/src/autostart"
	"discord-volume-toggle/src/config"
	"discord-volume-toggle/src/gui"
	"discord-volume-toggle/src/hotkey"
	"discord-volume-toggle/src/volume"
)

func main() {
	// Set up file logging so crashes can be diagnosed.
	logFile := setupLogging()
	if logFile != nil {
		defer logFile.Close()
	}

	// Recover from any panic so we can log the stack trace instead of dying
	// silently (or showing a cryptic Windows crash dialog).
	defer func() {
		if r := recover(); r != nil {
			log.Printf("PANIC: %v", r)
			// Log the stack trace.
			buf := make([]byte, 1<<16)
			n := runtime.Stack(buf, false)
			log.Printf("stack trace:\n%s", buf[:n])
		}
	}()

	// Single-instance guard: if another copy is already running, ask it to
	// show its window and exit this copy immediately.
	if gui.SingleInstance() {
		log.Printf("another instance is already running; showing its window and exiting")
		return
	}

	// Load config.
	cfg, err := config.Load()
	if err != nil {
		log.Printf("warning: could not load config: %v", err)
		cfg = config.Default()
	}

	// Wire volume package logging to our logger.
	volume.Log = func(format string, args ...interface{}) {
		log.Printf("[volume] "+format, args...)
	}
	config.Log = func(format string, args ...interface{}) {
		log.Printf("[config] "+format, args...)
	}
	// Wire GUI diagnostics/panic logging to our logger too.
	gui.Log = func(format string, args ...interface{}) {
		log.Printf("[gui] "+format, args...)
	}
	// Emergency direct-write path for the watchdog (bypasses the log
	// package's mutex so a hung main thread can't block stack dumps).
	if logFile != nil {
		gui.EmergencyWriter = func(b []byte) {
			_, _ = logFile.Write(b)
		}
	}

	// Initialize COM once (on the main thread, which runs the message loop and
	// therefore the hotkey toggle).
	if err := volume.Initialize(); err != nil {
		log.Printf("warning: COM init: %v", err)
	}

	// Create the hotkey manager (bound to the window once it exists).
	hk := hotkey.NewManager(0)

	// Create the GUI app.
	app := gui.NewApp(hk)

	// Toggle state: index into cfg.Levels.
	levelIndex := 0

	// Toggle handler: cycle Discord volume through the configured levels.
	app.SetToggleHandler(func() {
		if len(cfg.Levels) == 0 {
			app.SetStatus("Error: no volume levels configured")
			return
		}
		level := cfg.Levels[levelIndex]
		levelIndex = (levelIndex + 1) % len(cfg.Levels)

		log.Printf("toggling %s volume to %f", cfg.ProcessName, level)
		if err := volume.SetAppVolume(cfg.ProcessName, level); err != nil {
			log.Printf("toggle error: %v", err)
			app.SetStatus(fmt.Sprintf("Error: %v", err))
			return
		}
		pct := int(level * 100)
		app.SetStatus(fmt.Sprintf("Discord volume set to %d%%", pct))
		app.SetVolumeLabel(fmt.Sprintf("Volume: %d%%", pct))
		app.SetTrayVolume(pct)
		log.Printf("toggle complete (Discord at %d%%)", pct)
	})

	// Keybind handler: persist the new keybind and re-register the hotkey.
	app.SetKeybindHandler(func(k hotkey.Key) {
		log.Printf("new keybind captured: %s", k.String())
		if err := hk.Register(k); err != nil {
			log.Printf("register error: %v", err)
			app.SetStatus(fmt.Sprintf("Error: %v", err))
			return
		}
		cfg.KeybindVK = k.VK
		cfg.KeybindMods = k.Mods
		if err := config.Save(cfg); err != nil {
			log.Printf("config save warning: %v", err)
			app.SetStatus(fmt.Sprintf("Warning: could not save config: %v", err))
			return
		}
		app.SetKeybindLabel(fmt.Sprintf("Current keybind: %s", k.String()))
	})

	// Quit handler.
	app.SetQuitHandler(func() {
		hk.Unregister()
	})

	// Autostart handler: enable/disable the registry Run key.
	app.SetAutostartHandler(func(checked bool) {
		if checked {
			if err := autostart.Enable(); err != nil {
				log.Printf("autostart enable error: %v", err)
				app.SetStatus(fmt.Sprintf("Error enabling autostart: %v", err))
				app.SetAutostartChecked(false)
				return
			}
			app.SetStatus("Will run on startup.")
		} else {
			if err := autostart.Disable(); err != nil {
				log.Printf("autostart disable error: %v", err)
				app.SetStatus(fmt.Sprintf("Error disabling autostart: %v", err))
				return
			}
			app.SetStatus("Will not run on startup.")
		}
	})

	// Ready handler: fires after the window is created, so the hotkey can be
	// registered against the real window handle.
	app.SetReadyHandler(func() {
		// Set the initial keybind label.
		initial := hotkey.Key{VK: cfg.KeybindVK, Mods: cfg.KeybindMods}
		app.SetKeybindLabel(fmt.Sprintf("Current keybind: %s", initial.String()))

		// Set the initial autostart checkbox state.
		app.SetAutostartChecked(autostart.IsEnabled())

		// Register the initial hotkey.
		if err := hk.Register(initial); err != nil {
			log.Printf("initial hotkey register warning: %v", err)
			app.SetStatus(fmt.Sprintf("Warning: %v", err))
		} else {
			app.SetStatus("Ready. Press your keybind to toggle Discord volume.")
		}
	})

	// Run the message loop (blocks until quit).
	if err := app.Run(); err != nil {
		log.Printf("fatal: %v", err)
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	log.Printf("=== Discord Volume Toggle exited normally ===")
}

// setupLogging opens a log file in %APPDATA%\DiscordVolumeToggle\app.log and
// redirects the standard logger to it (plus stderr).
func setupLogging() *os.File {
	appData, err := os.UserConfigDir()
	if err != nil {
		return nil
	}
	dir := filepath.Join(appData, "DiscordVolumeToggle")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(dir, "app.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil
	}
	log.SetOutput(f)
	// Also route stdout/stderr into the log file: windowsgui apps have no
	// console, so anything written there (Go runtime errors, stray prints)
	// would otherwise vanish without a trace.
	os.Stdout = f
	os.Stderr = f
	setStdHandle := windows.NewLazySystemDLL("kernel32.dll").NewProc("SetStdHandle")
	setStdHandle.Call(0xFFFFFFF4, f.Fd()) // STD_ERROR_HANDLE
	setStdHandle.Call(0xFFFFFFF5, f.Fd()) // STD_OUTPUT_HANDLE
	log.SetFlags(log.LstdFlags)
	log.Printf("=== Discord Volume Toggle started ===")
	return f
}
