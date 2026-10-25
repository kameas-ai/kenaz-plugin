package variant_test

import (
	"os"
	"testing"
)

// buildTags are the tags the binary under test is built with.
var buildTags []string

// publicConfigDir is the public build's directory under kameas/.
const publicConfigDir = "kenaz"

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
