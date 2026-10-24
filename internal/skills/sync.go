package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"sort"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
)

// Report summarizes the last sync for status (counts and fixed strings
// only; slugs are catalog names the member can already see).
type Report struct {
	At         time.Time `json:"at"`
	Applied    int       `json:"applied"`
	Failed     int       `json:"failed"`
	Removed    int       `json:"removed"`
	Stripped   []string  `json:"stripped,omitempty"`   // "<slug>: allowed-tools, hooks"
	Translated []string  `json:"translated,omitempty"` // slugs translated from legacy skills
	Skipped    []string  `json:"skipped,omitempty"`    // "<slug or id>: <detail>"
}

// Syncer applies the Fleet config bundle's mandated items and the member's
// subscriptions to the harness roots.
type Syncer struct {
	Provider  fleetapi.Provider
	Store     Store
	Mat       *Materializer
	MachineID string
	Log       *slog.Logger
	Now       func() time.Time
}

func (s *Syncer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Syncer) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Sync runs one pass. It holds the skills lock for its duration.
func (s *Syncer) Sync(ctx context.Context) (Report, error) {
	unlock, err := s.Store.Lock(10 * time.Second)
	if err != nil {
		return Report{}, err
	}
	defer unlock()

	st, err := s.Store.LoadSyncState()
	if err != nil {
		return Report{}, err
	}
	sess, err := s.Provider.Auth().Session(ctx)
	if err != nil {
		return Report{}, err
	}
	owner := ownerKey(sess)
	if st.Owner != "" && st.Owner != owner {
		// Another account signed in: org content and the previous member's
		// subscriptions do not carry over.
		if err := s.dropAccount(); err != nil {
			return Report{}, err
		}
		st = SyncState{}
	}
	// An empty Owner (first sync, or state from an earlier build) is
	// adopted: earlier builds remove managed skills on logout, so what is
	// installed belongs to the account still signed in.
	st.Owner = owner
	bundle, err := s.pull(ctx, st)
	if err != nil {
		return Report{}, err
	}
	subs, err := s.Store.Subscriptions()
	if err != nil {
		return Report{}, err
	}

	rep := Report{At: s.now().UTC()}
	changed := false
	var acks []fleetapi.ConfigAck
	keep := map[string]bool{}
	mandated := map[string]string{}

	for _, it := range bundle.Mandated {
		if bundle.NotModified {
			// Unchanged and present on disk: keep it, nothing to write or ack.
			keep[it.CatalogID] = true
			mandated[it.CatalogID] = it.Version
			continue
		}
		if it.Kind != fleetapi.CatalogKindAgentSkill && it.Kind != fleetapi.CatalogKindSkill {
			continue // other kinds (workflows, packs) are not for third-party harnesses
		}
		keep[it.CatalogID] = true
		mandated[it.CatalogID] = it.Version
		res := s.Mat.Install(Item{
			CatalogID: it.CatalogID, Kind: it.Kind, Version: it.Version, Payload: it.Payload,
			Source: SourceMandated, ReviewedSHA256: it.ReviewedSHA256,
		})
		changed = changed || res.Changed
		tally(&rep, res)
		acks = append(acks, fleetapi.ConfigAck{CatalogID: it.CatalogID, Version: it.Version, Status: res.Status, Detail: res.Detail})
	}

	for id, sub := range subs {
		if keep[id] {
			continue // mandated wins; the subscription stays for later
		}
		item, err := s.Provider.Catalog().Get(ctx, id, sub.Version)
		if err != nil {
			if isNotFound(err) {
				delete(subs, id) // gone from the catalog: sweep below
				rep.Skipped = append(rep.Skipped, label(sub.Slug, id)+": "+DetailRevoked)
				continue
			}
			keep[id] = true // transient: leave what is installed
			s.log().Warn("skills: subscription fetch failed", "err", err.Error())
			continue
		}
		if item.Revoked {
			delete(subs, id)
			rep.Skipped = append(rep.Skipped, label(sub.Slug, id)+": "+DetailRevoked)
			continue
		}
		keep[id] = true
		res := s.Mat.Install(Item{
			CatalogID: id, Kind: item.Kind, Slug: item.Slug, Version: item.Version,
			Payload: item.Payload, Source: SourceSubscribed,
		})
		changed = changed || res.Changed
		tally(&rep, res)
	}

	// Revocation sweep: managed directories for items that are
	// neither mandated nor subscribed any more.
	owned, err := s.Mat.Owned()
	if err != nil {
		return rep, err
	}
	removedIDs := map[string]bool{}
	for _, o := range owned {
		if keep[o.Marker.CatalogID] {
			continue
		}
		if err := removeOwned(o.Dir); err != nil {
			s.log().Warn("skills: remove failed", "err", err.Error())
			continue
		}
		changed = true
		if !removedIDs[o.Marker.CatalogID] {
			removedIDs[o.Marker.CatalogID] = true
			rep.Removed++
		}
	}
	// Previously mandated items that left the bundle are acked removed.
	if !bundle.NotModified {
		for id, ver := range st.Mandated {
			if _, still := mandated[id]; !still {
				acks = append(acks, fleetapi.ConfigAck{CatalogID: id, Version: ver, Status: fleetapi.AckRemoved, Detail: DetailNoLongerMandated})
			}
		}
	}

	if bundle.ID != "" && len(acks) > 0 {
		if err := s.Provider.Pollers().AckConfigs(ctx, bundle.ID, acks); err != nil {
			s.log().Warn("skills: ack failed", "err", err.Error())
		}
	}
	if err := s.Store.SaveSubscriptions(subs); err != nil {
		return rep, err
	}
	st.Checksum = bundle.Checksum
	st.Mandated = mandated
	st.Report = &rep
	if err := s.Store.SaveSyncState(st); err != nil {
		return rep, err
	}
	if changed {
		if err := s.Store.BumpGeneration(); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// pull fetches the bundle. On 304 it re-pulls in full if any previously
// mandated item is missing on disk, so a user-deleted mandated skill is
// re-materialized (non-goal "mandated does not mean enforced": it comes
// back, but nothing is locked).
func (s *Syncer) pull(ctx context.Context, st SyncState) (fleetapi.ConfigBundle, error) {
	req := fleetapi.ConfigsRequest{MachineID: s.MachineID, Checksum: st.Checksum}
	b, err := s.Provider.Pollers().Configs(ctx, req)
	if err != nil || !b.NotModified {
		return b, err
	}
	owned, err := s.Mat.Owned()
	if err != nil {
		return b, err
	}
	present := map[string]int{}
	for _, o := range owned {
		present[o.Marker.CatalogID]++
	}
	for id := range st.Mandated {
		if present[id] < len(s.Mat.Roots) {
			req.Checksum = ""
			return s.Provider.Pollers().Configs(ctx, req)
		}
	}
	// Unchanged: reconstruct a bundle that keeps every mandated item.
	return fleetapi.ConfigBundle{NotModified: true, Checksum: st.Checksum, Mandated: keepMandated(st.Mandated)}, nil
}

// keepMandated lists the last-applied mandated items for a 304 pass, so
// the sweep keeps their directories. They carry no payload; Sync never
// installs from a NotModified bundle.
func keepMandated(mandated map[string]string) []fleetapi.MandatedItem {
	ids := make([]string, 0, len(mandated))
	for id := range mandated {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]fleetapi.MandatedItem, 0, len(ids))
	for _, id := range ids {
		out = append(out, fleetapi.MandatedItem{CatalogID: id, Version: mandated[id]})
	}
	return out
}

func tally(rep *Report, res Result) {
	name := label(res.Slug, res.CatalogID)
	switch res.Status {
	case fleetapi.AckApplied:
		rep.Applied++
	default:
		rep.Failed++
		rep.Skipped = append(rep.Skipped, name+": "+res.Detail)
	}
	if len(res.Stripped) > 0 {
		s := name + ": "
		for i, k := range res.Stripped {
			if i > 0 {
				s += ", "
			}
			s += k
		}
		rep.Stripped = append(rep.Stripped, s)
	}
	if res.Translated && res.Status == fleetapi.AckApplied {
		rep.Translated = append(rep.Translated, name)
	}
}

// isNotFound reports a Fleet 404/410 for a catalog item.
func isNotFound(err error) bool {
	var ae *fleetapi.APIError
	return errors.As(err, &ae) && (ae.Status == 404 || ae.Status == 410)
}

func label(slug, id string) string {
	if slug != "" {
		return slug
	}
	return id
}

// ownerKey identifies the account skills were synced for (Fleet env,
// organization, user) without storing the identifiers.
func ownerKey(s fleetapi.Session) string {
	h := sha256.Sum256([]byte("kenaz/skills-owner/v1\x00" + string(s.Env) + "\x00" + s.OrgID + "\x00" + s.UserID))
	return hex.EncodeToString(h[:16])
}

// dropAccount removes every managed skill directory and resets the
// subscriptions and sync state. The caller holds the skills lock.
func (s *Syncer) dropAccount() error {
	owned, err := s.Mat.Owned()
	if err != nil {
		return err
	}
	var errs []error
	for _, o := range owned {
		errs = append(errs, removeOwned(o.Dir))
	}
	errs = append(errs, s.Store.Reset())
	if len(owned) > 0 {
		errs = append(errs, s.Store.BumpGeneration())
	}
	return errors.Join(errs...)
}
