package skills

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
)

// fakeFleet is a test provider: configs bundle, acks and a catalog.
type fakeFleet struct {
	fleetapi.Provider // stub for everything else

	mu     sync.Mutex
	bundle fleetapi.ConfigBundle
	acks   []fleetapi.ConfigAck
	items  map[string]fleetapi.CatalogItem
	pulls  int
	gone   map[string]bool
}

func newFake() *fakeFleet {
	return &fakeFleet{Provider: stub.New(), items: map[string]fleetapi.CatalogItem{}, gone: map[string]bool{}}
}

func (f *fakeFleet) Pollers() fleetapi.Pollers { return fakePollers{f} }
func (f *fakeFleet) Catalog() fleetapi.Catalog { return fakeCatalog{f} }

type fakePollers struct{ f *fakeFleet }

func (p fakePollers) PendingApprovals(context.Context) ([]fleetapi.Approval, error) { return nil, nil }
func (p fakePollers) LockdownStatus(context.Context) (fleetapi.LockdownState, error) {
	return fleetapi.LockdownState{}, nil
}
func (p fakePollers) LockdownWait(context.Context) (fleetapi.LockdownState, bool, error) {
	return fleetapi.LockdownState{}, false, nil
}
func (p fakePollers) OrgPause(context.Context) (fleetapi.PauseState, error) {
	return fleetapi.PauseState{}, nil
}
func (p fakePollers) Configs(_ context.Context, req fleetapi.ConfigsRequest) (fleetapi.ConfigBundle, error) {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	p.f.pulls++
	if req.Checksum != "" && req.Checksum == p.f.bundle.Checksum {
		return fleetapi.ConfigBundle{NotModified: true}, nil
	}
	return p.f.bundle, nil
}
func (p fakePollers) AckConfigs(_ context.Context, _ string, acks []fleetapi.ConfigAck) error {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	p.f.acks = append(p.f.acks, acks...)
	return nil
}

type fakeCatalog struct{ f *fakeFleet }

func (c fakeCatalog) List(_ context.Context, q fleetapi.CatalogQuery) ([]fleetapi.CatalogEntry, error) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	var out []fleetapi.CatalogEntry
	for _, it := range c.f.items {
		if q.Kind == "" || q.Kind == it.Kind {
			out = append(out, it.CatalogEntry)
		}
	}
	return out, nil
}
func (c fakeCatalog) Get(_ context.Context, id, _ string) (fleetapi.CatalogItem, error) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	if c.f.gone[id] {
		return fleetapi.CatalogItem{}, &fleetapi.APIError{Status: 404, Code: "not_found"}
	}
	it, ok := c.f.items[id]
	if !ok {
		return fleetapi.CatalogItem{}, &fleetapi.APIError{Status: 404, Code: "not_found"}
	}
	return it, nil
}
func (c fakeCatalog) Publish(context.Context, fleetapi.PublishRequest) (fleetapi.CatalogEntry, error) {
	return fleetapi.CatalogEntry{}, fleetapi.ErrUnofficialBuild
}

func (f *fakeFleet) setBundle(id, checksum string, items ...fleetapi.MandatedItem) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bundle = fleetapi.ConfigBundle{ID: id, Checksum: checksum, Mandated: items}
}

func (f *fakeFleet) takeAcks() []fleetapi.ConfigAck {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.acks
	f.acks = nil
	return a
}

// skillPayload builds an agent_skill/v1 payload.
func skillPayload(t *testing.T, name string, extraFrontmatter string, files map[string]string) json.RawMessage {
	t.Helper()
	fm := "---\nname: " + name + "\ndescription: Review pull requests carefully.\n" + extraFrontmatter + "---\n\n# " + name + "\n\nDo the thing.\n"
	all := map[string]string{ManifestName: fm}
	for k, v := range files {
		all[k] = v
	}
	b, err := json.Marshal(AgentSkill{Schema: SchemaV1, Name: name, Description: "Review pull requests carefully.", Files: all})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustContain(t *testing.T, s, sub string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Fatalf("%q does not contain %q", s, sub)
	}
}
