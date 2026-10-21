package consent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/notices"
)

// the notice never claims the analytics floor before Fleet
// advertises it, and the small-org sentence appears only for 2–4 members.
func TestNoticeEditionSelection(t *testing.T) {
	cases := []struct {
		floor bool
		band  string
		want  string
	}{
		{false, notices.OrgSizeStandard, notices.AgentTelemetryV1ID},
		{false, notices.OrgSizeSmall, notices.AgentTelemetryV1ID},
		{true, notices.OrgSizeUnknown, notices.AgentTelemetryV1ID},
		{true, notices.OrgSizeSole, notices.AgentTelemetryV1ID},
		{true, notices.OrgSizeSmall, notices.AgentTelemetryV2SmallID},
		{true, notices.OrgSizeStandard, notices.AgentTelemetryV2ID},
	}
	const small = "Because your organization is small, administrators see only organization-wide totals."
	const noIndividual = "They do not see your individual sessions in analytics."
	const maySee = "In a small team, totals may effectively show your own activity."
	for _, c := range cases {
		e := notices.AgentTelemetry(c.floor, c.band)
		if e.ID != c.want {
			t.Errorf("floor=%v band=%q: got %s want %s", c.floor, c.band, e.ID, c.want)
		}
		switch e.ID {
		case notices.AgentTelemetryV1ID:
			if !strings.Contains(e.Text, maySee) || strings.Contains(e.Text, noIndividual) {
				t.Errorf("v1 wording wrong")
			}
		case notices.AgentTelemetryV2ID:
			if !strings.Contains(e.Text, noIndividual) || strings.Contains(e.Text, small) || strings.Contains(e.Text, maySee) {
				t.Errorf("v2 wording wrong")
			}
		case notices.AgentTelemetryV2SmallID:
			if !strings.Contains(e.Text, noIndividual+" "+small) {
				t.Errorf("v2-small wording wrong")
			}
		}
	}
}

// When Fleet starts advertising the floor, a member who accepted v1 is
// asked to re-confirm before anything more is sent.
func TestNoticeEditionChangeForcesReconfirm(t *testing.T) {
	f := newFake(allClasses()...)
	f.me.OrgSizeBand = notices.OrgSizeStandard
	store := NewStore(filepath.Join(t.TempDir(), "c.json"))
	m := NewManager(Options{Provider: f, ProjectionVersion: 1, Store: store})
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	optIn(t, m, fleetapi.ClassUsageCounts)
	if got := f.puts[len(f.puts)-1].NoticeVersion; got != NoticeVersion(notices.AgentTelemetryV1ID, 1) {
		t.Fatalf("accepted under %q, want v1", got)
	}

	f.set(func(f *fakeFleet) { f.caps.Keys[CapAnalyticsMemberFloor] = true })
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := m.Decide(fleetapi.ClassUsageCounts); d.Action != ActionDrop || d.Reason != ReasonNoticeReconfirm {
		t.Fatalf("edition change must block until re-confirmed: %+v", d)
	}
	if e := m.NoticeEdition(); e.ID != notices.AgentTelemetryV2ID {
		t.Fatalf("edition = %s", e.ID)
	}
	optIn(t, m, fleetapi.ClassUsageCounts)
	if got := f.puts[len(f.puts)-1].NoticeVersion; got != NoticeVersion(notices.AgentTelemetryV2ID, 1) {
		t.Fatalf("re-confirmed under %q", got)
	}
	if d := m.Decide(fleetapi.ClassUsageCounts); d.Action != ActionSend {
		t.Fatalf("after re-confirm: %+v", d)
	}
}
