//go:build windows

package pathenv

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Registry is the user environment in HKCU\Environment.
type Registry struct{}

// SystemUserEnv returns the registry-backed user environment.
func SystemUserEnv() (UserEnv, error) { return Registry{}, nil }

func (Registry) Path() (string, bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Environment`, registry.QUERY_VALUE)
	if err != nil {
		return "", true, err
	}
	defer k.Close()
	v, typ, err := k.GetStringValue("Path")
	if errors.Is(err, registry.ErrNotExist) {
		return "", true, nil
	}
	if err != nil {
		return "", true, err
	}
	return v, typ == registry.EXPAND_SZ, nil
}

func (Registry) SetPath(value string, expand bool) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Environment`, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if expand {
		return k.SetExpandStringValue("Path", value)
	}
	return k.SetStringValue("Path", value)
}

// Broadcast sends WM_SETTINGCHANGE("Environment") so Explorer, and the
// terminals it starts afterwards, pick up the new Path.
func (Registry) Broadcast() {
	const (
		hwndBroadcast   = 0xffff
		wmSettingChange = 0x001A
		smtoAbortIfHung = 0x0002
	)
	env, err := windows.UTF16PtrFromString("Environment")
	if err != nil {
		return
	}
	proc := windows.NewLazySystemDLL("user32.dll").NewProc("SendMessageTimeoutW")
	var result uintptr
	_, _, _ = proc.Call(hwndBroadcast, wmSettingChange, 0, uintptr(unsafe.Pointer(env)), smtoAbortIfHung, 5000, uintptr(unsafe.Pointer(&result)))
}
