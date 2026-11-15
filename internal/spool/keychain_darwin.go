//go:build darwin

package spool

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Storing an item in the macOS keychain runs `/usr/bin/security -i`, which
// blocks for good in a home with no usable login keychain. go-keyring
// starts it without a context, so nothing could interrupt it, and a caller
// that gave up left it running. Here the same helper gets the same command
// (so items either wrote are read by both), in its own process group, and
// when the call's context ends the whole group is killed: the call
// returns and nothing is left running.
//
// Reading and deleting stay go-keyring's (keychain_keyring.go). Its lookup
// answers at once in that home, and this package does not spell the
// helper's lookup command: no source in this repository may, so that
// nothing here can read another product's keychain items (the capture
// guard test enforces it).

// securityPath is the helper. A var for tests.
var securityPath = "/usr/bin/security"

const (
	// keychainBase64Prefix is what go-keyring puts before what it stores.
	keychainBase64Prefix = "go-keyring-base64:"
	// keychainMaxCommand is the longest line `security -i` reads.
	keychainMaxCommand = 4096
)

func keychainSet(ctx context.Context, service, item, secret string) error {
	// The secret goes to the helper on stdin, never on a command line
	// other processes can read.
	secret = keychainBase64Prefix + base64.StdEncoding.EncodeToString([]byte(secret))
	command := fmt.Sprintf("add-generic-password -U -s %s -a %s -w %s\n", shellQuote(service), shellQuote(item), shellQuote(secret))
	if len(command) > keychainMaxCommand {
		return errors.New("spool: the keychain item is too long to store")
	}
	cmd := exec.CommandContext(ctx, securityPath, "-i")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// The helper and anything it started.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// A child that keeps the output pipe open must not hold Wait up.
	cmd.WaitDelay = time.Second
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.Stdin = strings.NewReader(command)
	err := cmd.Run()
	if ctx.Err() != nil {
		return fmt.Errorf("%w (%v)", ErrKeychainTimeout, ctx.Err())
	}
	return err
}

var shellUnsafe = regexp.MustCompile(`[^\w@%+=:,./-]`)

// shellQuote quotes s for the helper's command reader the way a shell
// expects (single quotes, with each single quote closed, escaped and
// reopened); a string of safe characters is left as it is.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !shellUnsafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
