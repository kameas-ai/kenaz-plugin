package app

import (
	"os"
	"path/filepath"

	"github.com/kameas-ai/kenaz-plugin/internal/config"
)

// sessionLockFile is the session lock's name in the plugin's state root. It is a
// dotfile, so the layout doesn't count it as state (it never keeps an earlier
// release's files from being moved) and it means nothing by being there.
const sessionLockFile = ".session.lock"

// SessionLockPath is where the lock that serialises sign-in, sign-out and token
// refresh across this program's processes belongs: in the plugin's own state
// root (~/.kenaz/plugin, or $KENAZ_HOME/plugin, or an earlier release's files
// where they still are), beside the per-environment folders that hold each
// relay's daemon.lock. It is one path for every environment, because the
// provider is built before the environment is known and one provider serves
// whichever is signed in to.
//
// It is for the program that builds the provider, before it calls Main: the
// official build passes it to its Fleet client, whose own default for the OS
// keychain is a file under the user's configuration directory, outside these
// folders, that ignores KENAZ_HOME and that nothing removes. The path is the
// same in every process that reads the same environment variables. It only
// reads the environment and the layout on disk; it creates nothing.
func SessionLockPath() (string, error) {
	p, err := config.Layout(os.Getenv)
	if err != nil {
		return "", err
	}
	return filepath.Join(p.Current().StateRoot, sessionLockFile), nil
}
