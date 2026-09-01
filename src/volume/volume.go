// Package volume controls per-application output volume on Windows via the
// Core Audio API, using the tested github.com/moutend/go-wca bindings.
package volume

import (
	"fmt"
	"strings"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
	"golang.org/x/sys/windows"
)

// Log is a hook for debug logging (set by the caller).
var Log func(format string, args ...interface{})

func logf(format string, args ...interface{}) {
	if Log != nil {
		Log(format, args...)
	}
}

// Initialize initializes COM on the calling thread. Call this once at startup
// (on the thread that will call SetAppVolume).
func Initialize() error {
	return ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED)
}

// SetAppVolume sets the master volume (0.0 - 1.0) of the audio session whose
// process name matches the given process name (case-insensitive, e.g.
// "Discord.exe"). Returns an error if no matching session is found.
//
// NOTE: COM must already be initialized on the calling thread (the app does
// this once at startup). We do NOT call CoInitialize/CoUninitialize here,
// because doing so on every toggle unbalances the COM apartment and crashes.
func SetAppVolume(processName string, level float32) error {
	// Create the MMDeviceEnumerator.
	var enumerator *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(
		wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL,
		wca.IID_IMMDeviceEnumerator, &enumerator,
	); err != nil {
		return fmt.Errorf("CoCreateInstance(MMDeviceEnumerator) failed: %v", err)
	}
	defer enumerator.Release()
	logf("enumerator created")

	// Get the default render device.
	var device *wca.IMMDevice
	if err := enumerator.GetDefaultAudioEndpoint(wca.ERender, wca.EConsole, &device); err != nil {
		return fmt.Errorf("GetDefaultAudioEndpoint failed: %v", err)
	}
	defer device.Release()
	logf("default render device obtained")

	// Activate IAudioSessionManager2.
	var sessionManager *wca.IAudioSessionManager2
	if err := device.Activate(wca.IID_IAudioSessionManager2, wca.CLSCTX_ALL, nil, &sessionManager); err != nil {
		return fmt.Errorf("Activate(IAudioSessionManager2) failed: %v", err)
	}
	defer sessionManager.Release()
	logf("session manager activated")

	// Get the session enumerator.
	var sessionEnum *wca.IAudioSessionEnumerator
	if err := sessionManager.GetSessionEnumerator(&sessionEnum); err != nil {
		return fmt.Errorf("GetSessionEnumerator failed: %v", err)
	}
	defer sessionEnum.Release()

	// Get the session count.
	var count int
	if err := sessionEnum.GetCount(&count); err != nil {
		return fmt.Errorf("GetCount failed: %v", err)
	}
	logf("session count = %d", count)

	target := strings.ToLower(processName)

	for i := 0; i < count; i++ {
		var sessionControl *wca.IAudioSessionControl
		if err := sessionEnum.GetSession(i, &sessionControl); err != nil {
			continue
		}

		// Query for IAudioSessionControl2 to get the process ID.
		var sessionControl2 *wca.IAudioSessionControl2
		if err := sessionControl.PutQueryInterface(wca.IID_IAudioSessionControl2, &sessionControl2); err == nil && sessionControl2 != nil {
			var pid uint32
			if err := sessionControl2.GetProcessId(&pid); err == nil && pid != 0 {
				name := processNameFromPID(pid)
				logf("session %d: pid=%d name=%q", i, pid, name)
				if strings.ToLower(name) == target {
					// Found it. Get ISimpleAudioVolume.
					var simpleVolume *wca.ISimpleAudioVolume
					if err := sessionControl.PutQueryInterface(wca.IID_ISimpleAudioVolume, &simpleVolume); err == nil && simpleVolume != nil {
						err = simpleVolume.SetMasterVolume(level, nil)
						simpleVolume.Release()
						sessionControl2.Release()
						sessionControl.Release()
						if err != nil {
							return fmt.Errorf("SetMasterVolume failed: %v", err)
						}
						logf("volume set to %f for %q", level, name)
						return nil
					}
				}
			}
			sessionControl2.Release()
		}
		sessionControl.Release()
	}

	return fmt.Errorf("no audio session found for process %q (is Discord running and playing audio?)", processName)
}

// processNameFromPID returns the base name of the executable for a process ID.
func processNameFromPID(pid uint32) string {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(handle)

	var buf [windows.MAX_PATH]uint16
	size := uint32(len(buf))
	err = windows.QueryFullProcessImageName(handle, 0, &buf[0], &size)
	if err != nil {
		return ""
	}
	full := windows.UTF16ToString(buf[:size])
	if idx := strings.LastIndexAny(full, `\/`); idx >= 0 {
		return full[idx+1:]
	}
	return full
}
