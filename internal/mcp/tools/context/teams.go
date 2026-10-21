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
		if t, ok := match(want, teams); ok {
			return t, nil
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

func match(want string, teams []fleetapi.TeamRef) (fleetapi.TeamRef, bool) {
	for _, t := range teams {
		if t.ID == want {
			return t, true
		}
	}
	var hit []fleetapi.TeamRef
	for _, t := range teams {
		if strings.EqualFold(t.Name, want) {
			hit = append(hit, t)
		}
	}
	if len(hit) == 1 {
		return hit[0], true
	}
	return fleetapi.TeamRef{}, false
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
