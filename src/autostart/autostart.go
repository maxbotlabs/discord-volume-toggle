// Package autostart manages the "run on startup" setting via the Windows
// registry Run key (per-user, no admin required).
package autostart

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows/registry"
)

const (
	runKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`
	valueName  = "DiscordVolumeToggle"
)

// Enable adds (or updates) the Run key entry pointing at the current
// executable, so the app launches on login.
func Enable() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("could not determine executable path: %w", err)
	}

	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("could not open Run key: %w", err)
	}
	defer k.Close()

	// Quote the path in case it contains spaces.
	cmd := fmt.Sprintf(`"%s"`, exe)
	if err := k.SetStringValue(valueName, cmd); err != nil {
		return fmt.Errorf("could not set Run key: %w", err)
	}
	return nil
}

// Disable removes the Run key entry.
func Disable() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		// If the key doesn't exist, there's nothing to remove.
		return nil
	}
	defer k.Close()

	if err := k.DeleteValue(valueName); err != nil {
		// If the value doesn't exist, that's fine.
		if err == registry.ErrNotExist {
			return nil
		}
		return fmt.Errorf("could not delete Run key: %w", err)
	}
	return nil
}

// IsEnabled reports whether the Run key entry currently exists.
func IsEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()

	_, _, err = k.GetStringValue(valueName)
	return err == nil
}
