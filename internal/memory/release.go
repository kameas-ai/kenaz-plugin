package memory

// ProjectScopeRelease marks the release in which Kameas Fleet syncs project
// notes, and memory-sync/v4 replaces v3. It changes what forget-all does on an
// account's other devices, and what the approval page says about it, so that
// both always match the notice in force. The app sets it from the build.
var ProjectScopeRelease = false

// eraseScopes lists the scopes a pulled forget-all deletes from a device. This is
// the one place that decides it.
func eraseScopes() string {
	if ProjectScopeRelease {
		return "'global','long_term','project'"
	}
	return "'global','long_term'"
}

// Forget-all wording for the approval page.
const (
	forgetAllSummaryV4 = "Erase all your memory notes?"
	forgetAllImpactV4  = "This permanently deletes every note saved under your account, including project and session notes, in Kameas Fleet and on all your devices, including notes on other devices that never synced. " + ForgetAllInterim + " This can't be undone. Export your notes first if you may want them."
	// ForgetAllInterim is memory-sync/v4's account, word for word, of what other
	// devices do after a forget-all, until the fixes that let it promise more ship.
	// The notice and the approval page both carry it.
	ForgetAllInterim   = "Your other plugin devices erase these notes when they next sync, which may be later if sync is off there or your organization's plan is paused or has lapsed. A device running the Kenaz Harness keeps the notes on it for now. A device that hasn't synced before, or has a wrong clock, could send a note back to Kameas Fleet; you can delete it with `kenaz memory forget` or by asking your AI tool to forget it."
	forgetAllSummaryV3 = "Delete every memory of this Kameas Fleet account, on this device and in Kameas Fleet"
	// ForgetAllOtherDevices is the forget-all disclosure while project notes do
	// not sync: an erase reaches the account's other devices, including notes
	// there that were never synced, and leaves project notes there.
	ForgetAllOtherDevices = "This also erases your account's notes on your other devices when they next connect, including notes there that hadn't been synced yet. Notes that belong to a single project are kept."
)

// ForgetAllSummary is the approval page's headline for forget-all.
func ForgetAllSummary() string {
	if ProjectScopeRelease {
		return forgetAllSummaryV4
	}
	return forgetAllSummaryV3
}

// ForgetAllImpact is what the approval page says forget-all does. deleted is how
// many notes it deletes on this device.
func ForgetAllImpact(deleted int) string {
	if ProjectScopeRelease {
		return forgetAllImpactV4
	}
	return "Irreversible: erases all of this account's memories on this device and its memory in Kameas Fleet. " + ForgetAllOtherDevices
}
