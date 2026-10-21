package fleetapi

import "context"

// Member is a team roster entry, reduced to what the context tools show.
type Member struct {
	DisplayName string
	Role        string
}

// Team reads team rosters (GET /api/v1/team/members).
type Team interface {
	// Members lists teamID's members. teamID must be one of the caller's
	// MeInfo.Teams.
	Members(ctx context.Context, teamID string) ([]Member, error)
}
