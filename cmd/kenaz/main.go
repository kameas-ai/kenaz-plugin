// Command kenaz is the public (unofficial) build of Kenaz for
// Claude Code and Codex. It runs everything on the device but cannot
// connect to Kameas Fleet. Official builds come from a separate release
// pipeline, which calls app.Main with the official provider.
package main

import (
	"os"

	"github.com/kameas-ai/kenaz-plugin/app"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
)

func main() { os.Exit(app.Main(stub.New())) }
