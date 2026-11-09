package app

import (
	"context"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/features"
)

// Team sharing runs only on context_files, present and true, from an official build
// that is signed in and could read Fleet's answer. The local tools are served in
// every build with the library, whatever the answer.
func TestContextFilesOfferedParsing(t *testing.T) {
	old := features.ContextLibrary
	features.ContextLibrary = true
	t.Cleanup(func() { features.ContextLibrary = old })
	for _, c := range []struct {
		keys map[string]bool
		want bool
	}{
		{nil, false},
		{map[string]bool{"team_graph_sharing": true, fleetapi.CapMemorySync: true}, false},
		{map[string]bool{fleetapi.CapContextFiles: false}, false},
		{map[string]bool{fleetapi.CapContextFiles: true}, true},
		{map[string]bool{"CONTEXT_FILES": true}, true},
	} {
		env := Env{Provider: capsProvider{Provider: stub.New(), keys: c.keys}}
		if got := env.contextFilesOffered(context.Background()); got != c.want {
			t.Errorf("%v: %v", c.keys, got)
		}
		local, team := env.contextFilesGroups(context.Background())
		if len(local) != 1 || (len(team) > 0) != c.want {
			t.Errorf("%v: local %d, team %d", c.keys, len(local), len(team))
		}
	}
	for name, p := range map[string]fleetapi.Provider{
		"unofficial": unofficialCaps{capsProvider{Provider: stub.New(), keys: map[string]bool{fleetapi.CapContextFiles: true}}},
		"signed out": stub.New(),
		"read fails": capsErrProvider{capsProvider{Provider: stub.New()}},
	} {
		if (Env{Provider: p}).contextFilesOffered(context.Background()) {
			t.Errorf("%s: offered", name)
		}
	}
	features.ContextLibrary = false
	if (Env{Provider: capsProvider{Provider: stub.New(), keys: map[string]bool{fleetapi.CapContextFiles: true}}}).contextFilesOffered(context.Background()) {
		t.Error("a build without the library")
	}
	if local, team := (Env{Provider: stub.New()}).contextFilesGroups(context.Background()); len(local) != 0 || len(team) != 0 {
		t.Error("a build without the library serves context tools")
	}
}
