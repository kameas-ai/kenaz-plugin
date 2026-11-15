//go:build windows

package installed

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// abortSignals are the signals that end a journey early (Ctrl-C and
// Ctrl-Break; Windows has no SIGHUP).
var abortSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

// ownGroup starts cmd as the root of a new process group, so that a
// Ctrl-C sent to the journey's console group reaches the journey first.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

// guardProcess puts the started cmd in a job object that kills it when the
// journey's last handle to the job closes, which the operating system does
// when the journey exits for any reason, including being terminated. It is
// the Windows counterpart of the Unix watchdog for the relay the journey
// starts. Failing to create the job is not fatal: stopRelays still runs on
// every return of Run.
func guardProcess(cmd *exec.Cmd) (release func()) {
	none := func() {}
	if cmd == nil || cmd.Process == nil {
		return none
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return none
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return none
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return none
	}
	err = windows.AssignProcessToJobObject(job, h)
	_ = windows.CloseHandle(h)
	if err != nil {
		_ = windows.CloseHandle(job)
		return none
	}
	// Closing the handle kills whatever is still in the job.
	return func() { _ = windows.CloseHandle(job) }
}

// killGroup kills cmd and every process it started.
func killGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	killTree(cmd.Process.Pid)
	_ = cmd.Process.Kill()
}

// killTree kills pid and its descendants. The caller has verified that pid
// runs the sandbox's binary.
func killTree(pid int) {
	if pid <= 4 {
		return
	}
	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
}

// processExe returns the path of the executable pid runs, and whether pid
// is alive.
func processExe(pid int) (exe string, alive bool) {
	if pid <= 0 {
		return "", false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil || code != 259 { // STILL_ACTIVE
		return "", false
	}
	buf := make([]uint16, 32768)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return "", true
	}
	return windows.UTF16ToString(buf[:n]), true
}

// startWatchdog is the Unix watchdog; on Windows the job object in
// guardProcess covers the relay the journey starts. A relay that `kenaz
// mcp` spawned detached, and the temporary directory, are not cleaned up
// when the journey itself is terminated on Windows.
func (s *sandbox) startWatchdog() (release func()) { return func() {} }
