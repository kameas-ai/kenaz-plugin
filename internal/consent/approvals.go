package consent

import (
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
)

// ApprovalEffect is how one pending approval affects the telemetry lane.
type ApprovalEffect int

const (
	// EffectInformational shows the item and changes nothing.
	EffectInformational ApprovalEffect = iota
	// EffectHold holds all uploads (spool) until the item clears.
	EffectHold
)

// ClassifyApproval applies these rules:
//   - blocking legal_acceptance or blocking unknown kinds → hold;
//   - a blocking ml_notice never holds the telemetry lane;
//   - required, non-blocking, past deferrable_until → hold;
//   - everything else is informational.
func ClassifyApproval(a fleetapi.Approval, now time.Time) ApprovalEffect {
	if a.Kind == fleetapi.ApprovalKindMLNotice {
		return EffectInformational
	}
	if a.Blocking {
		switch a.Kind {
		case fleetapi.ApprovalKindMLExclusions, fleetapi.ApprovalKindMLOptInAvailable:
			// Known informational ML kinds: dismissing them is not consent.
			return EffectInformational
		}
		return EffectHold // legal_acceptance and any unknown blocking kind
	}
	if a.Required && a.DeferrableUntil != nil && now.After(*a.DeferrableUntil) {
		return EffectHold
	}
	return EffectInformational
}
