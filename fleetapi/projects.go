package fleetapi

import (
	"context"
	"errors"
)

// Project registry seam (Fleet contract docs/contract-harness-projects.md). A project
// note leaves the device with one Fleet-owned id. The registry holds identity only: an
// id, a display name, an audience and a state. It is additive like the memory seam: an
// implementation opts in by also implementing ProjectsProvider.

// ProjectRouteCreate is the one route this seam uses.
const ProjectRouteCreate = "POST /api/v1/projects"

// Project states. A replay of a client_request_id returns the existing row whatever
// its state, so a caller reads it.
const (
	ProjectActive  = "active"
	ProjectDeleted = "deleted"
	ProjectMerged  = "merged"
)

// ProjectAudiencePrivate is the only audience this seam asks for.
const ProjectAudiencePrivate = "private"

// ProjectCreateRequest is the POST body.
type ProjectCreateRequest struct {
	Name            string `json:"name"`
	Audience        string `json:"audience"`
	ClientRequestID string `json:"client_request_id"`
}

// Project is the part of the response this client reads.
type Project struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	MergedInto string `json:"merged_into,omitempty"`
}

// Projects is the registry, for the active session. Calls follow the same account
// binding as Memory: the implementation compares ExpectedSessionFrom(ctx) with the
// credentials it picked and refuses a mismatch with CodeIdentityMismatch, sending
// nothing.
type Projects interface {
	Create(ctx context.Context, req ProjectCreateRequest) (Project, error)
}

// ProjectsProvider is implemented by a Provider that can reach the registry.
type ProjectsProvider interface {
	Projects() Projects
}

// ErrProjectsUnavailable is returned by ProjectsOf's fallback.
var ErrProjectsUnavailable = errors.New("the project registry is not available in this build")

// ProjectsOf returns p's registry client, or one whose every call fails with
// ErrProjectsUnavailable.
func ProjectsOf(p Provider) Projects {
	if pp, ok := p.(ProjectsProvider); ok {
		if c := pp.Projects(); c != nil {
			return c
		}
	}
	return unavailableProjects{}
}

type unavailableProjects struct{}

func (unavailableProjects) Create(context.Context, ProjectCreateRequest) (Project, error) {
	return Project{}, ErrProjectsUnavailable
}
