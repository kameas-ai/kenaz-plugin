package contexttools

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/approvals"
	"github.com/kameas-ai/kenaz-plugin/internal/contextlib"
)

// The context share grant: once a person has turned it on at a terminal
// (`kenaz context grant on`), context_share_file shares a file without an approval of
// its own. Nothing in this package gives the grant; it only reads it.

// OrgAllowsShareGrant is where an organization's setting from Kameas Fleet will be
// able to forbid the context share grant. Fleet has no such setting yet, so today it
// always allows, and asks Fleet nothing. When it says no, `kenaz context grant on`
// refuses and a grant already given is ignored. A var so tests can forbid it.
var OrgAllowsShareGrant = func(context.Context, fleetapi.Provider) bool { return true }

// ShareGrantOptions are what the relay needs to run a share under the grant.
type ShareGrantOptions struct {
	Grant    contextlib.ShareGrant
	Ledger   contextlib.Ledger
	Provider fleetapi.Provider
	Now      func() time.Time
}

// AutoApproveShareFile decides whether a prepared file share runs under the grant;
// the relay then runs it through the same approval a person gives. It says no, which
// leaves the request pending for the person, when the organization forbids the grant,
// when this account hasn't given it, when the member isn't in the team, when the
// request is one the grant never covers (grantCovers), and when the hour's cap is
// spent. A yes is written to the history first: no record, no share.
//
// The one Fleet read (who the member is and their teams) happens before the lock, and
// is bounded by ctx; the lock covers only the count and the history file, so shares
// don't queue behind each other's uploads.
func AutoApproveShareFile(o ShareGrantOptions) approvals.AutoApprover {
	if o.Now == nil {
		o.Now = time.Now
	}
	var mu sync.Mutex
	var hour int64
	var spent int // this relay's own count, so that losing the history can't reset the cap
	return func(ctx context.Context, who approvals.Identity, id string, payload []byte) (func(approvals.Status), bool) {
		acct := contextlib.Account{Env: who.Env, OrgID: who.OrgID, UserID: who.UserID}
		if !OrgAllowsShareGrant(ctx, o.Provider) {
			return nil, false
		}
		if _, on := o.Grant.On(acct); !on {
			return nil, false
		}
		// Any team the member is in is covered, the organization's all-members team
		// included. One they aren't in is decided here, before a slot or a history
		// line is used.
		me, err := o.Provider.Me().Get(ctx)
		if err != nil || ctx.Err() != nil {
			return nil, false
		}
		sp, slug, ok := grantCovers(o.Ledger, acct, me, payload)
		if !ok {
			return nil, false
		}
		mu.Lock()
		defer mu.Unlock()
		now := o.Now()
		if h := now.Unix() / 3600; h != hour {
			hour, spent = h, 0
		}
		if used := o.Grant.UsedThisHour(acct, now); used > spent {
			spent = used
		}
		if spent >= contextlib.MaxGrantedSharesPerHour {
			return nil, false
		}
		e := contextlib.ShareAudit{
			Account: acct, Slug: slug, TeamID: sp.Node.TeamID, SHA256: contextlib.HashOf([]byte(sp.Node.Body)),
			At: now, RequestID: id, Status: string(approvals.StatusExecuting),
		}
		for _, t := range me.Teams {
			if t.ID == sp.Node.TeamID {
				e.TeamName = t.Name
			}
		}
		if o.Grant.Record(e) != nil {
			return nil, false
		}
		spent++
		return func(st approvals.Status) {
			mu.Lock()
			defer mu.Unlock()
			_ = o.Grant.Settle(id, string(st))
		}, true
	}
}

// grantCovers reports whether the grant may run this payload. It doesn't take the
// caller's word for what the tool checked: the relay scans the text again for
// credentials, hidden characters and note ids, and requires what the ledger holds for
// the file, as the tool does: no memory mark, the same path and short name, the
// member in the team, and either this account's reservation of this node and team (a
// new share) or its link to them, authored by this member, at the next version and
// over the text it last shared (an update).
//
// What it can't check is that the text is the file's: the relay doesn't read the
// file. A caller of the control API that can also write the ledger could have other
// text shared under the grant. Such a caller can write the file too.
func grantCovers(led contextlib.Ledger, acct contextlib.Account, me fleetapi.MeInfo, payload []byte) (shareFilePayload, string, bool) {
	sp, slug, err := decodeShareFile(payload)
	if err != nil || sp.FromMemory {
		// Text marked as drafted from memory gets the person's own look.
		return sp, "", false
	}
	if !memberOf(me.Teams, sp.Node.TeamID) {
		return sp, "", false
	}
	rec, err := led.Get(sp.LedgerKey)
	if err != nil || rec.DraftedFromMemory {
		return sp, "", false
	}
	if want, err := contextlib.SlugOf(rec.Path); err != nil || rec.Path != sp.Path || want != slug {
		return sp, "", false
	}
	if _, terr := checkShareText(slug, sp.Node.Body); terr != nil {
		return sp, "", false
	}
	if sp.Update {
		l, ok := rec.LinkFor(acct)
		return sp, slug, ok && l.Mine(me.UserID) && l.NodeID == sp.Node.ID && l.TeamID == sp.Node.TeamID &&
			l.BaseSHA256 == sp.BaseSHA256 && sp.Node.Version == l.Version+1
	}
	h, ok := rec.ReservationFor(acct)
	return sp, slug, ok && h.NodeID == sp.Node.ID && h.TeamID == sp.Node.TeamID && sp.Node.Version == 1
}

func memberOf(teams []fleetapi.TeamRef, id string) bool {
	for _, t := range teams {
		if t.ID == id {
			return true
		}
	}
	return false
}

// ShareGrantDescription is what context_share_file's description says about the grant.
var ShareGrantDescription = fmt.Sprintf("Exception: if the user has turned on the context share grant (`kenaz context grant on`), this call shares the file with the team at once, with no approval, up to %d files per clock hour, and the result says so: tell the user what was shared. "+
	"It covers every team the user is in, including the organization's all-members team (usually named Everyone): a share with that team is readable by the whole organization, so say so when you tell the user. "+
	"Text marked as drafted from memory notes (from_memory, here or when it was saved with context_write) still needs approval. The grant can't tell memory-derived text that isn't marked, so always set from_memory when any of the text came from memory notes. Only the user can turn the grant on, at a terminal; no tool can, so never try to and never ask them to for your own convenience.", contextlib.MaxGrantedSharesPerHour)

// wholeOrgNote is what a share's result says, once the file is shared with team,
// about the whole organization reading it. everyone and known are
// SharedWithWholeOrg's answer for the team: Kameas Fleet marks the organization's
// all-members team, which can be renamed, so the name is never what decides.
// Marked, the result says so outright; another team is marked, it says nothing;
// a Fleet that marks none (an older server) can't tell, and the result says what
// holds either way.
func wholeOrgNote(team string, everyone, known bool) string {
	switch {
	case known && everyone:
		return fmt.Sprintf("%s is the organization's all-members team, so everyone in the organization can now read it: say so. ", team)
	case !known:
		return fmt.Sprintf("If %s is the organization's all-members team, everyone in the organization can now read it: say so. ", team)
	}
	return ""
}

// shareFileImpact is who will be able to read a file once its share with team is
// approved: the approval's impact line, before the person approves.
func shareFileImpact(team string, everyone, known bool) string {
	if known && everyone {
		return fmt.Sprintf("Everyone in your organization will be able to read this file: %s is your organization's all-members team.", team)
	}
	return "Every member of " + team + " will be able to read this file."
}

// shareGrantNote is what a share's result says about the grant: that it published
// the file, or why a grant that is on left this one waiting. "" means the grant had no
// part in it. everyone and known say whether the team is the organization's
// all-members team (wholeOrgNote).
func shareGrantNote(ctx context.Context, ap approvals.Preparer, st StatusSource, pend approvals.Pending, slug, team string, everyone, known, fromMemory bool) (note string, ran bool) {
	const off = "They can see what it shared with `kenaz context grant status` and turn it off with `kenaz context grant off`."
	switch pend.Status {
	case "", approvals.StatusApprovalUnavailable:
		return "", false
	case approvals.StatusPending:
		if st == nil {
			return "", false
		}
		ds, err := st.Status(ctx)
		if err != nil || ds.ContextShareGrant == nil || !ds.ContextShareGrant.On {
			return "", false
		}
		why := "it doesn't cover this request"
		switch g := ds.ContextShareGrant; {
		case fromMemory:
			why = "it doesn't cover text marked as drafted from memory notes"
		case g.UsedThisHour >= g.HourlyCap:
			why = fmt.Sprintf("it has reached its limit of %d files per clock hour", g.HourlyCap)
		}
		return "The context share grant is on, but " + why + ", so this one waits for the user's approval as usual.", false
	}
	if ap == nil {
		return "", false
	}
	_, res, err := ap.Status(ctx, pend.RequestID)
	var meta struct {
		ApprovedBy string `json:"approved_by"`
	}
	if err != nil || json.Unmarshal(res, &meta) != nil || meta.ApprovedBy != approvals.ApprovedByGrant {
		return "", false
	}
	switch pend.Status {
	case approvals.StatusDone:
		return fmt.Sprintf("Shared without a separate approval, under the context share grant the user turned on: every member of %s can now read the file %q. %sTell the user it was shared. ", team, slug, wholeOrgNote(team, everyone, known)) + off, true
	case approvals.StatusExecuting, approvals.StatusReconciling:
		return fmt.Sprintf("Being shared with the team %s without a separate approval, under the context share grant the user turned on. It hasn't finished, so don't say the file %q is shared yet: check with approval_status, and tell the user. ", team, slug) + off, true
	}
	return "The context share grant ran this without a separate approval and it didn't go through: nothing was shared. Check approval_status for the reason. " + off, true
}
