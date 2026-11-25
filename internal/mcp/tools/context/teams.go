package contexttools

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/usertext"
)

// DefaultTeamKey is the config.toml key naming the user's default team (by
// id or name) for share and team-scoped search.
const DefaultTeamKey = "default_team"

// CodeNotTeamMember refuses a share to a team the member can read context from
// but isn't in. It is Kameas Fleet's own code for the same refusal, so a caller
// sees one code whether the plugin or Fleet said no.
const CodeNotTeamMember = "not_team_member"

// What the tools' descriptions say about teams a member can read and not share
// to. Agents read these, so they say what happens and what to pass.
const (
	shareOwnTeamsOnly = "You can share only with a team the user is a member of. Kameas Fleet may also let them read context from teams they are not a member of (membership \"inherited\" in team_members: teams above theirs, and teams below their own); naming one of those here is refused before anything is prepared, and the refusal lists the teams they can share to."
	readAnyTeam       = "team can be any team the user can read context from, including one they are not a member of (membership \"inherited\" in team_members)."
)

// Teams is the team list from /me, split by what the member may do with each
// team. Kameas Fleet lists the teams the member belongs to and, once reading
// context across the team tree is on, teams they can only read ("inherited":
// the teams above theirs and the teams below their own non-default teams).
//
// Every share target and every membership check in this package goes through
// Teams, so that an inherited team can be read from and never written to:
// ShareTarget, Member and ShareRefusal see only the member's own teams, and
// ReadTarget sees them all. Nothing else in the package ranges over
// MeInfo.Teams (TestTeamRowsAreReadThroughTeams).
type Teams struct {
	// rows is every team the member can read context from; own is those they
	// belong to (fleetapi.MemberTeams).
	rows, own []fleetapi.TeamRef
	// degraded says rows is not everything Fleet sent (fleetapi.TeamsDegraded).
	degraded fleetapi.TeamsDegraded
}

// TeamsOfMe is the member's teams from /me, with what the provider said about
// rows it could not hand over. It is how the tools build a Teams.
func TeamsOfMe(me fleetapi.MeInfo) Teams {
	ts := TeamsOf(me.Teams)
	ts.degraded = me.TeamsDegraded
	return ts
}

// HiddenNote is one sentence saying that Kameas Fleet sent teams this version of
// Kenaz could not read, and so are not listed, or "".
func (ts Teams) HiddenNote() string {
	return usertext.TeamsHidden(ts.degraded.Hidden(), ts.degraded.NotAList)
}

// TreeNote is one sentence saying some teams are listed without their place in
// the team tree, or "".
func (ts Teams) TreeNote() string { return usertext.TeamTreeUnread(ts.degraded.TreeDropped) }

// TeamsOf splits /me's team rows. A row with no membership (an older Fleet or
// client) is one of the member's own teams.
func TeamsOf(rows []fleetapi.TeamRef) Teams {
	return Teams{rows: rows, own: fleetapi.MemberTeams(rows)}
}

// Readable is every team the member can read context from: their own and the
// inherited ones.
func (ts Teams) Readable() []fleetapi.TeamRef { return ts.rows }

// Own is the teams the member belongs to: the only ones they can share to.
func (ts Teams) Own() []fleetapi.TeamRef { return ts.own }

// Member reports whether the member belongs to team id. An inherited team is not
// one they belong to.
func (ts Teams) Member(id string) bool {
	_, ok := find(ts.own, id)
	return ok
}

// readOnly is the inherited team with this id, if the member has one.
func (ts Teams) readOnly(id string) (fleetapi.TeamRef, bool) {
	if t, ok := find(ts.rows, id); ok && t.Inherited() {
		return t, true
	}
	return fleetapi.TeamRef{}, false
}

// Name is the display name of team id, among every team the member can read, or
// "". It is for naming a team, never for deciding anything about it.
func (ts Teams) Name(id string) string {
	t, _ := find(ts.rows, id)
	return t.Name
}

// OwnName is the display name of team id if the member belongs to it, or "". It
// is for a page that calls the team one of the person's own, or says they are
// sharing with it: a team they only read is never named that way.
func (ts Teams) OwnName(id string) string {
	t, _ := find(ts.own, id)
	return t.Name
}

// ApprovalRefusal is what an approval page says when the team a share names is
// not one the member belongs to, by what Fleet lists now: approving it will
// share nothing, because Fleet shares only with a team the member is in. "" when
// the member belongs to the team, and when no team is listed at all (Fleet out
// of reach), which says nothing either way.
func (ts Teams) ApprovalRefusal(id string) string {
	if len(ts.rows) == 0 || ts.Member(id) {
		return ""
	}
	return "You are not a member of the team with id " + id + ", so approving this will not share it: Kameas Fleet shares only with teams you belong to. Decline it, and share with one of your own teams."
}

// WholeOrg is SharedWithWholeOrg for these teams.
func (ts Teams) WholeOrg(teamID string) (everyone, known bool) {
	return SharedWithWholeOrg(ts.rows, teamID)
}

func find(teams []fleetapi.TeamRef, id string) (fleetapi.TeamRef, bool) {
	for _, t := range teams {
		if t.ID == id && id != "" {
			return t, true
		}
	}
	return fleetapi.TeamRef{}, false
}

// ShareTarget picks the team a share goes to: an explicit argument (id or
// case-insensitive name), else the configured default, else the member's only
// team. Only the member's own teams can be picked, so a name can never resolve to
// someone else's team, and a member of one team who can read several others
// still has exactly one team.
//
// A team the member can only read is refused by name, with the teams they can
// share to, whether it was asked for or is the configured default: a default that
// names it isn't stale, and falling through to another team would share somewhere
// the person didn't choose.
func (ts Teams) ShareTarget(arg, configured string) (fleetapi.TeamRef, *ToolError) {
	if len(ts.rows) == 0 {
		return fleetapi.TeamRef{}, ts.noTeams()
	}
	for i, want := range []string{arg, configured} {
		want = strings.TrimSpace(want)
		if want == "" {
			continue
		}
		hit := match(want, ts.own)
		if len(hit) == 1 {
			return hit[0], nil
		}
		if len(hit) > 1 {
			// Fleet keeps names unique in an organization, so this shouldn't
			// happen. If it does, don't pick one: a name is a label, an id is
			// the team.
			return fleetapi.TeamRef{}, ambiguousTeam(want, hit)
		}
		if ro := match(want, ts.rows); len(ro) > 0 {
			return fleetapi.TeamRef{}, ts.notMember(ro[0], i == 1)
		}
		if want == strings.TrimSpace(arg) {
			if len(ts.own) == 0 {
				return fleetapi.TeamRef{}, ts.noOwnTeams()
			}
			return fleetapi.TeamRef{}, teamChoices("No team named "+quote(want)+" in your memberships.", ts.own)
		}
		// A stale configured default falls through to the single-team rule.
	}
	switch len(ts.own) {
	case 0:
		return fleetapi.TeamRef{}, ts.noOwnTeams()
	case 1:
		return ts.own[0], nil
	}
	return fleetapi.TeamRef{}, teamChoices("You belong to more than one team; say which one.", ts.own)
}

// ShareRefusal is why team id can't be shared to, or nil when the member belongs
// to it. It is the check for a target that was settled earlier (a file already
// shared, an approved request, a folder's team) and has to hold still.
func (ts Teams) ShareRefusal(id string) *ToolError {
	if ts.Member(id) {
		return nil
	}
	if t, ok := ts.readOnly(id); ok {
		return ts.notMember(t, false)
	}
	return &ToolError{Code: failScopeChanged, Message: "You aren't in this team any more, so nothing was shared."}
}

// ReadTarget picks the team a read is about: an explicit argument (id or
// case-insensitive name), else the configured default, else the member's only
// team. Every team the member can read can be named, the inherited ones too.
// With nothing named, "your only team" is still the one team the member
// belongs to, however many others they can read.
func (ts Teams) ReadTarget(arg, configured string) (fleetapi.TeamRef, *ToolError) {
	if len(ts.rows) == 0 {
		return fleetapi.TeamRef{}, ts.noTeams()
	}
	// With no inherited rows these are the texts every earlier version gave.
	inherited := len(ts.own) != len(ts.rows)
	for _, want := range []string{arg, configured} {
		want = strings.TrimSpace(want)
		if want == "" {
			continue
		}
		hit := match(want, ts.rows)
		if len(hit) == 1 {
			return hit[0], nil
		}
		if len(hit) > 1 {
			return fleetapi.TeamRef{}, ambiguousTeam(want, hit)
		}
		if want == strings.TrimSpace(arg) {
			msg := "No team named " + quote(want) + " in your memberships."
			if inherited {
				msg = "No team named " + quote(want) + " among your teams or the teams whose context you can read."
			}
			return fleetapi.TeamRef{}, teamChoices(msg, ts.rows)
		}
		// A stale configured default falls through to the single-team rule.
	}
	switch {
	case len(ts.own) == 1:
		return ts.own[0], nil
	case len(ts.own) == 0 && len(ts.rows) == 1:
		return ts.rows[0], nil
	case !inherited:
		return fleetapi.TeamRef{}, teamChoices("You belong to more than one team; say which one.", ts.rows)
	case len(ts.own) == 0:
		return fleetapi.TeamRef{}, teamChoices("You can read context from more than one team; say which one.", ts.rows)
	}
	return fleetapi.TeamRef{}, teamChoices("You belong to more than one team, and can read context from others; say which one.", ts.rows)
}

// noTeams is for a member with no team listed at all. When Fleet sent teams this
// version couldn't read, that is what it says: such a member may well be in some,
// and must not be told they are in none and sent to their administrator.
func (ts Teams) noTeams() *ToolError {
	if note := ts.HiddenNote(); note != "" {
		return &ToolError{Code: CodeTeamRequired, Message: note + " Until then Kenaz can't tell which teams you are in.", Remedy: "Update Kenaz. If that doesn't help, tell your Kameas Fleet administrator what this message says."}
	}
	return &ToolError{Code: CodeTeamRequired, Message: "You are not a member of any team in Kameas Fleet.", Remedy: "Ask your Kameas Fleet administrator to add you to a team."}
}

// noOwnTeams is for a member who can read some teams' context and belongs to none
// that is listed.
func (ts Teams) noOwnTeams() *ToolError {
	if note := ts.HiddenNote(); note != "" {
		return &ToolError{Code: CodeTeamRequired, Message: note + " None of the teams Kenaz can read is one you belong to, so there is no team it can share to. You can still read context from the teams team_members lists.", Remedy: "Update Kenaz. If that doesn't help, tell your Kameas Fleet administrator what this message says."}
	}
	return &ToolError{Code: CodeTeamRequired, Message: "You are not a member of any team in Kameas Fleet, so there is no team you can share to. You can still read context from the teams team_members lists.", Remedy: "Ask your Kameas Fleet administrator to add you to a team."}
}

// maxTeamsNamed bounds how many teams a refusal lists.
const maxTeamsNamed = 12

// notMember is the refusal for sharing to a team the member can only read. It
// names that team and the teams they can share to, by name and id, and is made
// before anything is prepared: no approval, no reserved id, no grant slot.
// fromDefault says the team came from the default_team setting, not the call.
func (ts Teams) notMember(t fleetapi.TeamRef, fromDefault bool) *ToolError {
	own := append([]fleetapi.TeamRef(nil), ts.own...)
	sort.Slice(own, func(i, j int) bool {
		if own[i].Name != own[j].Name {
			return own[i].Name < own[j].Name
		}
		return own[i].ID < own[j].ID
	})
	ids := make([]string, 0, len(own))
	var named []string
	for i, o := range own {
		ids = append(ids, o.ID)
		if i < maxTeamsNamed {
			named = append(named, teamLabel(o))
		}
	}
	if extra := len(own) - len(named); extra > 0 {
		named = append(named, fmt.Sprintf("and %d more", extra))
	}
	msg := "You can read the context of " + teamLabel(t) + ", but you are not a member of that team, so you can't share to it. Nothing was shared. "
	if fromDefault {
		msg = "Your " + DefaultTeamKey + " setting names " + teamLabel(t) + ". You can read that team's context, but you are not a member of it, so you can't share to it. Nothing was shared. "
	}
	remedy := "Pass team with the id of one of your own teams; team_members lists them."
	switch {
	case len(own) == 0 && ts.HiddenNote() != "":
		msg += ts.HiddenNote() + " None of the teams Kenaz can read is one you belong to, so there is no team it can share to."
		remedy = "Update Kenaz. If that doesn't help, tell your Kameas Fleet administrator what this message says."
	case len(own) == 0:
		msg += "You are not a member of any team, so there is no team you can share to."
		remedy = "Ask your Kameas Fleet administrator to add you to a team."
	default:
		msg += "You can share to: " + strings.Join(named, "; ") + "."
		if fromDefault {
			remedy = "Pass team with the id of one of your own teams, or change " + DefaultTeamKey + " in the Kameas Fleet config."
		}
	}
	return &ToolError{Code: CodeNotTeamMember, Message: msg, Remedy: remedy, Choices: ids}
}

// teamLabel names a team for a message: its name and its id, since the id is
// what to pass and a name is only a label.
func teamLabel(t fleetapi.TeamRef) string {
	if t.Name == "" {
		return "the team with id " + t.ID
	}
	return t.Name + " (id " + t.ID + ")"
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

// ambiguousTeam is the refusal for a name more than one of the teams in hit
// has: the member's own, or for a read any they can read. It lists their ids, in the message too for callers that show only that.
func ambiguousTeam(want string, hit []fleetapi.TeamRef) *ToolError {
	ids := make([]string, 0, len(hit))
	for _, t := range hit {
		ids = append(ids, t.ID)
	}
	sort.Strings(ids)
	// "Your teams" only when every one of them is: a team the member only reads
	// isn't theirs.
	whose := "More than one of your teams is named "
	for _, t := range hit {
		if t.Inherited() {
			whose = "More than one of the teams you belong to or can read is named "
		}
	}
	return &ToolError{
		Code:    CodeTeamRequired,
		Message: whose + quote(want) + " (ids: " + strings.Join(ids, ", ") + ").",
		Remedy:  "Pass team with the id of the one you mean; team_members lists each team's id and parent.",
		Choices: ids,
	}
}

// SharedWithWholeOrg reports whether a share with team teamID is one everyone in
// the organization can read: the team is the one Fleet flagged as the default
// (all-members) team. known is false when Fleet flagged no team in teams (a Fleet
// that predates the flag): the caller then can't tell from /me and keeps what it
// did before, which is to assume nothing about any one team.
//
// It is for wording about who can read, and reads every row: Fleet lists the
// default team as an inherited row for a member with no membership row on it,
// and that still says which team is the all-members one. It never says the
// member can share to that team: Teams.ShareTarget and Teams.Member decide that.
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
