//go:build !windows

package pathenv

import "errors"

// SystemUserEnv is the Windows user environment; it does not exist here.
func SystemUserEnv() (UserEnv, error) {
	return nil, errors.New("the Windows user environment is only available on Windows")
}
