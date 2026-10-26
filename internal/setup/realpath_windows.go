//go:build windows

package setup

import (
	"strings"

	"golang.org/x/sys/windows"
)

// realPath resolves every link in p, junctions included (which
// filepath.EvalSymlinks leaves alone since Go 1.23): the final path of an
// open handle.
func realPath(p string) (string, error) {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return "", err
	}
	h, err := windows.CreateFile(name, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0) // VOLUME_NAME_DOS
	if err != nil {
		return "", err
	}
	s := windows.UTF16ToString(buf[:n])
	if strings.HasPrefix(s, `\\?\UNC\`) {
		return `\\` + s[len(`\\?\UNC\`):], nil
	}
	return strings.TrimPrefix(s, `\\?\`), nil
}
