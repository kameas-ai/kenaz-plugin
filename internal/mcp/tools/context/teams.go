package contexttools

import (
	"sort"
	"strings"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
)

// DefaultTeamKey is the config.toml key naming the user's default team (by
// id or name) for share and team-scoped search.
const DefaultTeamKey = "default_team"

// ResolveTeam picks the team for a call: an explicit
// argument (id or case-insensitive name), else the configured default, else
// the user's only team. teams are the caller's own memberships from /me,
// so a name can never resolve to someone else's team.
func ResolveTeam(arg, configured string, teams []fleetapi.TeamRef) (fleetapi.TeamRef, *ToolError) {
	if len(teams) == 0 {
		return fleetapi.TeamRef{}, &ToolError{Code: CodeTeamRequired, Message: "You are not a member of any team in Kameas Fleet.", Remedy: "Ask your Kameas Fleet administrator to add you to a team."}
	}
	for _, want := range []string{arg, configured} {
		want = strings.TrimSpace(want)
		if want == "" {
			continue
		}
		hit := match(want, teams)
		if len(hit) == 1 {
			return hit[0], nil
		}
		if len(hit) > 1 {
			// Fleet keeps names unique in an organization, so this shouldn't
			// happen. If it does, don't pick one: a name is a label, an id is
			// the team.
			return fleetapi.TeamRef{}, ambiguousTeam(want, hit)
		}
		if want == strings.TrimSpace(arg) {
			return fleetapi.TeamRef{}, teamChoices("No team named "+quote(want)+" in your memberships.", teams)
		}
		// A stale configured default falls through to the single-team rule.
	}
	if len(teams) == 1 {
		return teams[0], nil
	}
	return fleetapi.TeamRef{}, teamChoices("You belong to more than one team; say which one.", teams)
}

// match is the team with id want, else every team named want, ignoring case.
func match(want string, teams []fleetapi.TeamRef) []fleetapi.TeamRef {
	for _, t := range teams {
		if t.ID == want {
			return []fleetapi.TeamRef{t}
		}
	}
	var hit []fleetapi.TeamRef
	for _, t := range teams {
		if strings.EqualFold(t.Name, want) {
			hit = append(hit, t)
		}
	}
	return hit
}

// ambiguousTeam is the refusal for a name more than one of the member's teams
// has. It lists their ids, in the message too for callers that show only that.
func ambiguousTeam(want string, hit []fleetapi.TeamRef) *ToolError {
	ids := make([]string, 0, len(hit))
	for _, t := range hit {
		ids = append(ids, t.ID)
	}
	sort.Strings(ids)
	return &ToolError{
		Code:    CodeTeamRequired,
		Message: "More than one of your teams is named " + quote(want) + " (ids: " + strings.Join(ids, ", ") + ").",
		Remedy:  "Pass team with the id of the one you mean; team_members lists each team's id and parent.",
		Choices: ids,
	}
}

// SharedWithWholeOrg reports whether a share with team teamID is one everyone in
// the organization can read: the team is the one Fleet flagged as the default
// (all-members) team. known is false when Fleet flagged no team in teams (a Fleet
// that predates the flag): the caller then can't tell from /me and keeps what it
// did before, which is to assume nothing about any one team.
func SharedWithWholeOrg(teams []fleetapi.TeamRef, teamID string) (everyone, known bool) {
	def, ok := fleetapi.DefaultTeam(teams)
	if !ok {
		return false, false
	}
	return teamID != "" && def.ID == teamID, true
}

func teamChoices(msg string, teams []fleetapi.TeamRef) *ToolError {
	names := make([]string, 0, len(teams))
	for _, t := range teams {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	return &ToolError{Code: CodeTeamRequired, Message: msg, Remedy: "Pass team with one of the choices, or set " + DefaultTeamKey + " in the Kameas Fleet config.", Choices: names}
}

func quote(s string) string { return "\"" + s + "\"" }
