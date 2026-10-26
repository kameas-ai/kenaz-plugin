//go:build windows

package config

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// platformLink makes a directory link: a symbolic link where the user may
// create one (developer mode, an administrator), otherwise a junction,
// which needs no privilege.
func platformLink(target, link string) error {
	if os.Symlink(target, link) == nil {
		return nil
	}
	return createJunction(target, link)
}

const (
	fsctlSetReparsePoint   = 0x000900A4
	ioReparseTagMountPoint = 0xA0000003
)

// createJunction makes link a junction (mount point) to the directory
// target natively (FSCTL_SET_REPARSE_POINT): no shell is involved, so no
// character in either path needs quoting.
func createJunction(target, link string) error {
	abs, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return fmt.Errorf("junction target %s is not a directory", abs)
	}
	sub, err := windows.UTF16FromString(`\??\` + abs)
	if err != nil {
		return err
	}
	prn, err := windows.UTF16FromString(abs)
	if err != nil {
		return err
	}
	// MountPointReparseBuffer: SubstituteName, then PrintName, each
	// NUL-terminated; the lengths, in bytes, leave the NULs out.
	subLen, prnLen := (len(sub)-1)*2, (len(prn)-1)*2
	data := make([]byte, 8)
	binary.LittleEndian.PutUint16(data[0:], 0)                // SubstituteNameOffset
	binary.LittleEndian.PutUint16(data[2:], uint16(subLen))   // SubstituteNameLength
	binary.LittleEndian.PutUint16(data[4:], uint16(subLen+2)) // PrintNameOffset
	binary.LittleEndian.PutUint16(data[6:], uint16(prnLen))   // PrintNameLength
	for _, c := range append(append([]uint16{}, sub...), prn...) {
		data = binary.LittleEndian.AppendUint16(data, c)
	}
	buf := make([]byte, 8, 8+len(data))
	binary.LittleEndian.PutUint32(buf[0:], ioReparseTagMountPoint)
	binary.LittleEndian.PutUint16(buf[4:], uint16(len(data))) // ReparseDataLength
	buf = append(buf, data...)

	if err := os.Mkdir(link, 0o700); err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(link)
	if err != nil {
		os.Remove(link)
		return err
	}
	h, err := windows.CreateFile(name, windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		os.Remove(link)
		return err
	}
	var n uint32
	ioErr := windows.DeviceIoControl(h, fsctlSetReparsePoint, &buf[0], uint32(len(buf)), nil, 0, &n, nil)
	windows.CloseHandle(h)
	if ioErr != nil {
		os.Remove(link)
		return errors.New("create junction: " + ioErr.Error())
	}
	return nil
}

// crossDevice reports a rename refused because the two paths are on
// different volumes.
func crossDevice(err error) bool { return errors.Is(err, windows.ERROR_NOT_SAME_DEVICE) }
