package approvaltools

import (
	"context"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/internal/approvals"
)

type fixed struct {
	st  approvals.Status
	res string
}

func (fixed) Prepare(context.Context, approvals.Request) (string, approvals.Status, error) {
	return "", "", nil
}
func (f fixed) Status(context.Context, string) (approvals.Status, []byte, error) {
	return f.st, []byte(f.res), nil
}
func (fixed) ApprovalURL(context.Context, string) (string, error) { return "", nil }

// A share Kameas Fleet refused because the member isn't in the team says so: the
// generic line would leave an agent guessing, and it is nothing an admin lifts.
func TestStatusSaysNotTeamMember(t *testing.T) {
	const id = "apr_aaaaaaaaaaaaaaaaaaaaaaaaaa"
	out, err := Status(context.Background(), fixed{approvals.StatusFailed, `{"error":"not_team_member"}`}, StatusInput{RequestID: id})
	want := "Approved, but the share was refused because the user is not a member of that team. Nothing was shared. They can share only to teams they belong to; team_members lists them. See result.error."
	if err != nil || out.Message != want || string(out.Result) != `{"error":"not_team_member"}` {
		t.Fatalf("%+v %v", out, err)
	}
	// Any other failure keeps the generic line, and a code is only read from a failure.
	for _, f := range []fixed{
		{approvals.StatusFailed, `{"error":"node_changed"}`},
		{approvals.StatusFailed, ``},
		{approvals.StatusDone, `{"error":"not_team_member"}`},
	} {
		out, err := Status(context.Background(), f, StatusInput{RequestID: id})
		if err != nil || strings.Contains(out.Message, "not a member") || out.Message != messages[f.st] {
			t.Fatalf("%+v: %+v %v", f, out, err)
		}
	}
}
