// Package stub is the public-build fleetapi.Provider: every call fails with
// fleetapi.ErrUnofficialBuild. Official builds replace it with the private
// adapter.
package stub

import (
	"context"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
)

var errUnofficial = fleetapi.ErrUnofficialBuild

// New returns the unofficial-build provider.
func New() fleetapi.Provider { return provider{} }

type provider struct{}

func (provider) Flavor() fleetapi.Flavor     { return fleetapi.FlavorUnofficial }
func (provider) Auth() fleetapi.Auth         { return auth{} }
func (provider) Enroller() fleetapi.Enroller { return enroller{} }
func (provider) Me() fleetapi.Me             { return me{} }
func (provider) Pollers() fleetapi.Pollers   { return pollers{} }
func (provider) OptIns() fleetapi.OptIns     { return optIns{} }
func (provider) Catalog() fleetapi.Catalog   { return catalog{} }
func (provider) Context() fleetapi.Context   { return contextGraph{} }
func (provider) Team() fleetapi.Team         { return team{} }
func (provider) OTLP() fleetapi.OTLPPoster   { return otlp{} }

type auth struct{}

func (auth) Login(context.Context, fleetapi.LoginRequest) (fleetapi.Session, error) {
	return fleetapi.Session{}, errUnofficial
}
func (auth) Logout(context.Context) error { return errUnofficial }
func (auth) Session(context.Context) (fleetapi.Session, error) {
	return fleetapi.Session{}, errUnofficial
}
func (auth) TokenStore() fleetapi.TokenStore { return tokenStore{} }

type tokenStore struct{}

func (tokenStore) Clear(context.Context) error { return errUnofficial }

type enroller struct{}

func (enroller) Enroll(context.Context, fleetapi.EnrollBody) (fleetapi.EnrollResult, error) {
	return fleetapi.EnrollResult{}, errUnofficial
}

type me struct{}

func (me) Get(context.Context) (fleetapi.MeInfo, error) { return fleetapi.MeInfo{}, errUnofficial }
func (me) Capabilities(context.Context) (fleetapi.Capabilities, error) {
	return fleetapi.Capabilities{}, errUnofficial
}

type pollers struct{}

func (pollers) PendingApprovals(context.Context) ([]fleetapi.Approval, error) {
	return nil, errUnofficial
}
func (pollers) LockdownStatus(context.Context) (fleetapi.LockdownState, error) {
	return fleetapi.LockdownState{}, errUnofficial
}
func (pollers) LockdownWait(ctx context.Context) (fleetapi.LockdownState, bool, error) {
	// Don't spin a caller's long-poll loop: wait briefly, as a real
	// long-poll would, unless the context ends first.
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Millisecond):
	}
	return fleetapi.LockdownState{}, false, errUnofficial
}
func (pollers) OrgPause(context.Context) (fleetapi.PauseState, error) {
	return fleetapi.PauseState{}, errUnofficial
}
func (pollers) Configs(context.Context, fleetapi.ConfigsRequest) (fleetapi.ConfigBundle, error) {
	return fleetapi.ConfigBundle{}, errUnofficial
}
func (pollers) AckConfigs(context.Context, string, []fleetapi.ConfigAck) error {
	return errUnofficial
}

type optIns struct{}

func (optIns) Get(context.Context) ([]fleetapi.OptInState, error) { return nil, errUnofficial }
func (optIns) Put(context.Context, fleetapi.OptInPut) error       { return errUnofficial }

type catalog struct{}

func (catalog) List(context.Context, fleetapi.CatalogQuery) ([]fleetapi.CatalogEntry, error) {
	return nil, errUnofficial
}
func (catalog) Get(context.Context, string, string) (fleetapi.CatalogItem, error) {
	return fleetapi.CatalogItem{}, errUnofficial
}
func (catalog) Publish(context.Context, fleetapi.PublishRequest) (fleetapi.CatalogEntry, error) {
	return fleetapi.CatalogEntry{}, errUnofficial
}

type contextGraph struct{}

func (contextGraph) Search(context.Context, fleetapi.SearchRequest) ([]fleetapi.SearchResult, error) {
	return nil, errUnofficial
}
func (contextGraph) Pull(context.Context, fleetapi.PullRequest) ([]fleetapi.NodeContent, error) {
	return nil, errUnofficial
}
func (contextGraph) Push(context.Context, fleetapi.PushRequest) (fleetapi.ContextNode, error) {
	return fleetapi.ContextNode{}, errUnofficial
}
func (contextGraph) Promote(context.Context, fleetapi.PromoteRequest) (fleetapi.PromoteResult, error) {
	return fleetapi.PromoteResult{}, errUnofficial
}

type team struct{}

func (team) Members(context.Context, string) ([]fleetapi.Member, error) { return nil, errUnofficial }

type otlp struct{}

func (otlp) Post(context.Context, fleetapi.ProjectedBatch) (fleetapi.ExportResult, error) {
	return fleetapi.ExportResult{}, errUnofficial
}
