// Package fleetapi is the open-core seam between the public Kenaz
// plugin and the proprietary Kameas Fleet API client.
//
// Every interaction with Kameas Fleet goes through the interfaces in this
// package. There are two
// implementations:
//
//   - fleetapi/stub: the default in this public repository. Every call fails
//     with ErrUnofficialBuild. Public builds compile, test and run everything
//     on the device (receiver, projector, setup, plugin, MCP tool
//     definitions) but cannot connect to Kameas Fleet.
//   - the official adapter in the proprietary Fleet connection module. Only
//     official builds link it.
//     This module's go.mod never references it.
//
// # Payload rule
//
// Every Fleet-bound payload is built in public code before it crosses
// this seam: enroll and heartbeat bodies (EnrollBody) and fully projected
// OTLP batches (ProjectedBatch). An implementation may add only transport
// concerns: the bearer token, user agent and request id. It must not add,
// rename or enrich any body key, attribute or field. The closed set of keys
// the public layer can emit is exported by fleetapi/allowlist. Official builds'
// CI fails if the transport sends anything outside it.
//
// # No policy
//
// Nothing in this package receives, evaluates or applies Fleet policy
// (Cedar deltas, MCP allowlists, tool rules). The configs surface exposes
// mandated catalog items only.
//
// All request and response types are owned by this public package. No
// signature names a Fleet-internal type.
package fleetapi
