//go:build !windows

package installed

import (
	"os"
	"os/exec"
	"syscall"
)

// watchdogScript waits until the journey's end of the pipe on its standard
// input closes, which happens when the journey returns and also when it is
// killed without a chance to clean up. It then kills the relay the journey
// started and any relay recorded in the sandbox's daemon.json, each only
// while the operating system reports that the pid runs the sandbox's own
// binary ($2), and removes the sandbox ($1). After a normal return there is
// nothing left for it to do.
const watchdogScript = `
root=$1; exe=$2
while IFS= read -r _; do :; done
case "$root" in */kenaz-conformance-*) ;; *) exit 0 ;; esac
[ -d "$root" ] || exit 0
exe_of() {
	if [ -e "/proc/$1/exe" ]; then readlink "/proc/$1/exe" 2>/dev/null
	else ps -p "$1" -o comm= 2>/dev/null | sed -e 's/^ *//' -e 's/ *$//'
	fi
}
stop() {
	case "$1" in ''|*[!0-9]*|0|1) return ;; esac
	[ "$(exe_of "$1")" = "$exe" ] || return
	kill -9 -- "-$1" 2>/dev/null
	kill -9 "$1" 2>/dev/null
}
for round in 1 2 3; do
	[ -f "$root/relay.pid" ] && stop "$(tr -cd '0-9' < "$root/relay.pid")"
	for f in "$root"/home/.kenaz/plugin/*/daemon.json; do
		[ -f "$f" ] || continue
		stop "$(sed -n 's/.*"pid":[ ]*\([0-9][0-9]*\).*/\1/p' "$f" | head -n 1)"
	done
	sleep 1
done
rm -rf "$root"
`

// startWatchdog starts the watchdog in its own session, so that neither a
// signal to the journey's process group nor a closed terminal ends it, and
// returns the function that tells it the journey is over. The pipe's write
// end is close-on-exec: no child of the journey holds it open.
func (s *sandbox) startWatchdog() (release func()) {
	r, w, err := os.Pipe()
	if err != nil {
		return func() {}
	}
	cmd := exec.Command("/bin/sh", "-c", watchdogScript, "kenaz-conformance-watchdog", s.root, s.exe)
	cmd.Stdin = r
	cmd.Dir = "/"
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	_ = r.Close()
	if err != nil {
		_ = w.Close()
		return func() {}
	}
	return func() {
		_ = w.Close()
		_ = cmd.Wait()
	}
}
