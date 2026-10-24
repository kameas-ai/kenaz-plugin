package conformance

import (
	"context"
	"errors"
	"os"
	"os/exec"
)

func execLookPath(name string) (string, error) { return exec.LookPath(name) }

// execCommand runs name and returns its combined output and exit code.
// err is set only when the command could not run at all.
func execCommand(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out, ee.ExitCode(), nil
	}
	if err != nil {
		return out, -1, err
	}
	return out, 0, nil
}
