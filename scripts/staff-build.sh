#!/usr/bin/env bash
# Staff build of the official flavor with memory capture, project notes and context files, wrapped as
# an unsigned local bundle. Nothing is signed or published. See docs/memory-staff.md and
# docs/context-staff.md.
#
# usage: scripts/staff-build.sh <release checkout> <Fleet client checkout> <new output dir>
# with FLEET_DEV_CLIENT_ID and FLEET_PROD_CLIENT_ID set. It works on copies.
set -euo pipefail
src="$(cd "$(dirname "$0")/.." && pwd)"
release="$(cd "$1" && pwd)"; client="$(cd "$2" && pwd)"
mkdir -p "$(dirname "$3")"; out="$(cd "$(dirname "$3")" && pwd)/$(basename "$3")"
: "${FLEET_DEV_CLIENT_ID:?}" "${FLEET_PROD_CLIENT_ID:?}"
work="$(mktemp -d)"; trap 'rm -rf "$work"' EXIT
cp -R "$release" "$work/release"
cp -R "$client" "$work/client"
ln -s "$src" "$work/public"
public_mod="$(awk '/^module /{print $2; exit}' "$src/go.mod")"
client_mod="$(awk '/^module /{print $2; exit}' "$client/go.mod")"
# A client that doesn't implement the project registry yet gets it here.
if ! grep -rq ProjectsProvider "$work/client/adapter"; then
cat > "$work/client/adapter/projects.go" <<EOF
package adapter

import (
	"context"
	"net/http"

	"$public_mod/fleetapi"
)

var _ fleetapi.ProjectsProvider = (*Provider)(nil)

// Projects implements fleetapi.ProjectsProvider.
func (p *Provider) Projects() fleetapi.Projects { return projectsImpl{memoryImpl{p}} }

type projectsImpl struct{ m memoryImpl }

// Create implements fleetapi.Projects, bound to the expected session like memory.
func (x projectsImpl) Create(ctx context.Context, req fleetapi.ProjectCreateRequest) (fleetapi.Project, error) {
	var out fleetapi.Project
	err := x.m.send(ctx, http.MethodPost, "/api/v1/projects", req, &out)
	return out, err
}
EOF
fi
cd "$work/release"
export GOPRIVATE='github.com/kameas-ai/*' GONOSUMDB='github.com/kameas-ai/*' GOFLAGS=-mod=mod GOWORK=off
go mod edit -replace "$public_mod=$work/public" -replace "$client_mod=$work/client"
(cd "$work/client" && go mod edit -replace "$public_mod=$work/public")
go mod tidy >/dev/null 2>&1 || true
make licenses PUBLIC="$work/public" >/dev/null
sha="$(git -C "$src" rev-parse --short HEAD 2>/dev/null || echo local)"
P="$public_mod/app"; C="$client_mod/env"
go build -trimpath -tags fleet,kenaz_memory_capture,kenaz_memory_project_scope,kenaz_context_library -ldflags "-X $P.Version=0.8.1-staff.$sha -X $P.BuildCommit=$sha -X $P.PublicModuleVersion=staff -X $P.PrivateModuleVersion=staff -X $P.PrivateCommit=staff -X $C.devClientID=$FLEET_DEV_CLIENT_ID -X $C.prodClientID=$FLEET_PROD_CLIENT_ID" -o "$work/kenaz" ./cmd/kenaz
python3 "$src/scripts/staff-bundle.py" --binary "$work/kenaz" --out "$out" --suffix "staff.$sha"
