package fleetapi

// Flavor identifies which implementation a binary was built with.
// `kenaz doctor` and `--version` report it.
type Flavor string

const (
	FlavorOfficial   Flavor = "official"
	FlavorUnofficial Flavor = "unofficial"
)

// Env selects the Fleet realm (issuer, client id, API base URL). The
// implementation owns the per-env constants.
type Env string

const (
	EnvProd  Env = "prod"
	EnvStage Env = "stage"
	EnvDev   Env = "dev"
)

// Valid reports whether e is a known environment.
func (e Env) Valid() bool {
	switch e {
	case EnvProd, EnvStage, EnvDev:
		return true
	}
	return false
}

// Provider is the whole seam. app.Main receives exactly one Provider: the
// stub in public builds, the official adapter in release builds.
//
// One session (one Env) is active at a time. Every method except
// Auth().Login uses the active session and returns ErrNotSignedIn when
// there is none.
type Provider interface {
	// Flavor reports the build flavor. It must not perform I/O.
	Flavor() Flavor
	// Auth handles sign-in, sign-out and token storage.
	Auth() Auth
	// Enroller registers this device and serves as the heartbeat.
	Enroller() Enroller
	// Me reads the caller's identity, org state, teams and capabilities.
	Me() Me
	// Pollers reads approvals, lockdown, org pause and the configs bundle.
	Pollers() Pollers
	// OptIns reads and writes the member's telemetry class opt-ins.
	OptIns() OptIns
	// Catalog browses, fetches and publishes catalog items.
	Catalog() Catalog
	// Context reads and shares team/org context graph nodes.
	Context() Context
	// Team reads team rosters.
	Team() Team
	// OTLP posts fully projected telemetry batches.
	OTLP() OTLPPoster
}
