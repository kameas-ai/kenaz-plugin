// Package mcpb holds the Claude Desktop extension (MCP Bundle, .mcpb)
// manifest template and its icon (icon.png). The official release build
// renders the template once per platform and zips it with icon.png and the
// already-signed kenaz executable; this repository never builds a bundle
// itself.
//
// The template is valid JSON with three placeholders, each a whole token
// inside a JSON string, so any renderer is a plain string replacement:
//
//	@VERSION@   the release version, without the leading "v"
//	@PLATFORM@  the MCPB platform: darwin or win32
//	@EXE@       the bundled executable's file name: kenaz or kenaz.exe
package mcpb

import (
	_ "embed"
	"fmt"
	"regexp"
	"strings"

	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// Template is manifest.json.tmpl.
//
//go:embed manifest.json.tmpl
var Template string

// IconFile is the extension icon, next to the template here and at the
// bundle root in a release (the manifest's "icon").
const IconFile = "icon.png"

// Executables maps each supported MCPB platform to the executable file name
// the bundle carries under server/. Claude Desktop runs on macOS and
// Windows only.
var Executables = map[string]string{
	"darwin": variant.Current.Binary,
	"win32":  variant.Current.Binary + ".exe",
}

// Placeholder matches a template placeholder.
var Placeholder = regexp.MustCompile(`@[A-Z]+@`)

var version = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

// Render returns the manifest for one platform.
func Render(ver, platform string) ([]byte, error) {
	if !version.MatchString(ver) {
		return nil, fmt.Errorf("mcpb: version %q is not X.Y.Z[-pre]", ver)
	}
	exe, ok := Executables[platform]
	if !ok {
		return nil, fmt.Errorf("mcpb: unsupported platform %q", platform)
	}
	out := strings.NewReplacer("@VERSION@", ver, "@PLATFORM@", platform, "@EXE@", exe).Replace(Template)
	if p := Placeholder.FindString(out); p != "" {
		return nil, fmt.Errorf("mcpb: unknown placeholder %s in the template", p)
	}
	return []byte(out), nil
}
