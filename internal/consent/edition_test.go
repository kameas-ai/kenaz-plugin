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
		{true, notices.OrgSizeSmall, notices.AgentTelemetryV3SmallID},
		{true, notices.OrgSizeStandard, notices.AgentTelemetryV3ID},
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
		case notices.AgentTelemetryV3ID:
			if !strings.Contains(e.Text, noIndividual) || strings.Contains(e.Text, small) || strings.Contains(e.Text, maySee) {
				t.Errorf("v3 wording wrong")
			}
		case notices.AgentTelemetryV3SmallID:
			if !strings.Contains(e.Text, noIndividual+" "+small) {
				t.Errorf("v3-small wording wrong")
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
	if e := m.NoticeEdition(); e.ID != notices.AgentTelemetryV3ID {
		t.Fatalf("edition = %s", e.ID)
	}
	optIn(t, m, fleetapi.ClassUsageCounts)
	if got := f.puts[len(f.puts)-1].NoticeVersion; got != NoticeVersion(notices.AgentTelemetryV3ID, 1) {
		t.Fatalf("re-confirmed under %q", got)
	}
	if d := m.Decide(fleetapi.ClassUsageCounts); d.Action != ActionSend {
		t.Fatalf("after re-confirm: %+v", d)
	}
}

// v3 and v3-small reword v2 and v2-small without changing what is sent or
// who sees it (2026-10-25 revision): a member who opted in under
// v2 (or v2-small) keeps sending without being asked again, and is not
// re-recorded. v1 is not equivalent to v3.
func TestV2OptInsStandUnderV3(t *testing.T) {
	for _, c := range []struct {
		band string
		old  string
	}{
		{notices.OrgSizeStandard, notices.AgentTelemetryV2ID},
		{notices.OrgSizeSmall, notices.AgentTelemetryV2SmallID},
	} {
		f := newFake(allClasses()...)
		f.me.OrgSizeBand = c.band
		f.caps.Keys[CapAnalyticsMemberFloor] = true
		s := f.optins[fleetapi.ClassUsageCounts]
		s.OptedIn = true
		s.NoticeVersion = NoticeVersion(c.old, 1)
		f.optins[fleetapi.ClassUsageCounts] = s
		m := NewManager(Options{Provider: f, ProjectionVersion: 1, Store: NewStore(filepath.Join(t.TempDir(), "c.json"))})
		if err := m.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		if d := m.Decide(fleetapi.ClassUsageCounts); d.Action != ActionSend {
			t.Errorf("%s opt-in under %s: %+v, want send without re-confirmation", c.band, c.old, d)
		}
		puts := len(f.puts)
		if puts != 0 {
			t.Errorf("%s: opt-in re-recorded %d times", c.band, puts)
		}
	}

	// A v1 opt-in still re-confirms once the floor is advertised.
	f := newFake(allClasses()...)
	f.me.OrgSizeBand = notices.OrgSizeStandard
	f.caps.Keys[CapAnalyticsMemberFloor] = true
	s := f.optins[fleetapi.ClassUsageCounts]
	s.OptedIn = true
	s.NoticeVersion = NoticeVersion(notices.AgentTelemetryV1ID, 1)
	f.optins[fleetapi.ClassUsageCounts] = s
	m := NewManager(Options{Provider: f, ProjectionVersion: 1})
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := m.Decide(fleetapi.ClassUsageCounts); d.Reason != ReasonNoticeReconfirm {
		t.Errorf("v1 opt-in under v3: %+v, want re-confirmation", d)
	}
}
