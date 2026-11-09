// Package usertext holds the words a person sees when they turn something on or approve
// a write, so that they can be changed in one place. Notices live in the notices package.
package usertext

import "strconv"

// Confirmations. Enter, y or yes (in any case) means yes; anything else changes nothing,
// and nothing is recorded.
const (
	confirmHow = " Press Enter or y for yes; anything else for no: "

	PromptMemoryOn  = "Turn on memory, including capture and sync?" + confirmHow
	PromptSyncOn    = "Turn on sync?" + confirmHow
	PromptAutoOn    = "Turn on auto-sync?" + confirmHow
	PromptCaptureOn = "Turn on capture?" + confirmHow
	PromptRecallAll = "Let chat read every project?" + confirmHow
	PromptApprove   = "Approve this?" + confirmHow
	PromptDecline   = "Decline this?" + confirmHow

	// PromptForgetAll is its own wording: it can't be undone.
	PromptForgetAll = "This permanently erases every note under your account and can't be undone. Press Enter or y to erase; anything else cancels: "

	NothingChanged = "Nothing changed."
)

// NeedsTerminal is the refusal when stdin isn't a real terminal. command is what to run.
func NeedsTerminal(command string) string {
	return "nothing changed: this needs you at a terminal. Run: " + command
}

// One-sentence reasons and results for `kenaz memory on`.
const (
	MemoryAlreadyOn = "Memory is already on."
	QuotaReached    = "You've reached your Kameas Fleet memory limit, so new notes stay on this device and send when there's room."
	SignedOutSkip   = "Sync needs a sign-in first: run `kenaz login`, then this again."
	NoSyncInBuild   = "Sync isn't available in this build."
	SyncUnreadable  = "Sync's state couldn't be read, so it wasn't changed."
	// ProjectNotesNotOffered: this build's sync includes project notes, and Kameas
	// Fleet doesn't sync them for the organization (no memory_project_scope).
	ProjectNotesNotOffered = "Sync in this build includes project notes, which Kameas Fleet doesn't sync for your organization yet, so sync wasn't turned on."
	NoCaptureInBuild       = "Automatic capture isn't available in this build."
	CaptureOrgOff          = "Your organization has turned automatic capture off."
	// CaptureNotOffered: Kameas Fleet answered without memory_capture_kinds.
	CaptureNotOffered = "Automatic memory capture isn't available for your organization yet."
	// CaptureUnconfirmedForOrg: Kameas Fleet couldn't be asked (signed out, offline).
	CaptureUnconfirmedForOrg = "Kameas Fleet couldn't confirm automatic memory capture is available for your organization, so it stays off. Try again later."
	// ContextFilesNotOffered: Kameas Fleet doesn't send context_files for the
	// organization (or couldn't be asked), so the context library does nothing.
	ContextFilesNotOffered = "Context files aren't available for your organization yet."
	// CaptureSignedOutWriters: signed out, only the writers that reach no one but the
	// member's own provider run.
	CaptureSignedOutWriters = "While you're signed out, automatic memory capture can use only your own API key (`--writer api-key`) or a model on this device (`--writer local`). Sign in to use the others."
	// CaptureNeedsOfficialBuild: only an official build can hear Kameas Fleet's answer.
	CaptureNeedsOfficialBuild = "Automatic memory capture isn't available in this build."
	CaptureUnconfirmed        = "Kameas Fleet couldn't confirm automatic capture is allowed right now, so capture stays off. Run this again later."
	CheckingEarlierNotes      = "Checking claude-mem…"
	SyncPartly                = "Sync is on, but some notes haven't been sent. Run this again to send them."
	CaptureSwitchedOff        = "Automatic capture with your AI tool's login is switched off right now."
	CaptureDidntStart         = "Capture didn't turn on."
	AutoDidntStart            = "New notes will ask before syncing."
	ImportDidntFinish         = "Your earlier notes didn't all come over; running this again carries on."
	RestartForHooks           = "If your AI tool was already open, start a new session so capture picks up."
	RelayNextSession          = "Notes send when your AI tool next starts."
	RelayOtherBuild           = "Another Kenaz build's relay is running; notes send once it exits."
	MemoryOff                 = "Capture and sync are off. Your notes stay on this device, and any already synced stay in your account."
	MemoryOffFailed           = "capture or sync couldn't be turned off; check `kenaz memory capture status` and `kenaz memory sync status`."
)

// CaptureNeedsTool says what automatic capture is missing.
func CaptureNeedsTool(command string) string {
	return "Automatic capture needs `" + command + "` on your PATH, so it stays off."
}

// CaptureWith says how notes get written.
func CaptureWith(tool string) string {
	return "Capture uses your own " + tool + " login on this device."
}

// SyncUploads says what sync sends first.
func SyncUploads(n int) string {
	return plural(n, "note") + " on this device will upload."
}

// ImportTotals says what comes over from the earlier tool, before the question.
func ImportTotals(n int) string {
	return "This also brings over up to " + strconv.Itoa(n) + " notes from claude-mem. They'll sync to your Kameas Fleet account, and can include details from your past work."
}

// SyncRefusal is one plain sentence for why sync didn't turn on. code is Fleet's reason.
func SyncRefusal(code string) string {
	switch code {
	case "capability_not_in_tier":
		return "Your organization's plan doesn't include memory sync."
	case "org_paused":
		return "Your organization is paused in Kameas Fleet, so sync didn't turn on."
	case "lockdown", "emergency_lockdown":
		return "Your organization has locked down Kameas Fleet, so sync didn't turn on."
	case "memory_connector_forbidden", "memory_sync_forbidden":
		return "Your organization doesn't allow memory sync."
	case "":
		return "Kameas Fleet couldn't be reached, so sync didn't turn on. Run this again."
	}
	return "Kameas Fleet refused sync (" + code + ")."
}

// AutoWaitingToUpload is the count in `sync status` of automatic notes not yet sent.
func AutoWaitingToUpload(n int) string {
	return plural(n, "automatic note") + " waiting to upload."
}

// ImportLeftOut says how many notes stay behind in claude-mem, past the room there is.
func ImportLeftOut(n int) string {
	return plural(n, "note") + " left out to stay within your storage room."
}

// BlankLines stands in for a long run of blank lines in text shown for approval.
func BlankLines(n int) string {
	return "[" + strconv.Itoa(n) + " blank lines]"
}

// PromptSyncShare is the question before files go to a team.
func PromptSyncShare(n int, team string, fromMemory int) string {
	s := "Share these " + strconv.Itoa(n) + " files with " + team + "? Your team will be able to read their full text. "
	them := "them"
	if n == 1 {
		s, them = "Share this file with "+team+"? Your team will be able to read its full text. ", "it"
	}
	switch {
	case fromMemory == 1 && n == 1:
		s += "It's drawn from your private memory. "
	case fromMemory == 1:
		s += "1 is drawn from your private memory. "
	case fromMemory > 1:
		s += strconv.Itoa(fromMemory) + " are drawn from your private memory. "
	}
	return s + "Press v to view " + them + ", Enter or y to share, anything else to cancel: "
}

// Lines for `kenaz context sync`.
const (
	SyncNextFile        = "Press Enter for the next file, or q to stop viewing: "
	SyncUpToDate        = "Everything is up to date."
	SyncHint            = "To share this with your team, have the person run `kenaz context sync`."
	SyncNotYoursLine    = "only its author can change it on Kameas Fleet for now. Your text stays in your folder."
	SyncRemovedLine     = "it was removed from Kameas Fleet, so it wasn't sent. Your copy stays."
	SyncNameGone        = "a file with this name was removed from Kameas Fleet, so it can't be shared under this name. Rename it and run this again."
	SyncPromotedLine    = "it's now shared with the whole organization, so it can't be changed from here."
	SyncBlockedLine     = "Kameas Fleet blocked it because it may hold a credential or unsafe content."
	SyncUnreachLine     = "not confirmed, because Kameas Fleet didn't answer. Run sync again."
	SyncRefusedLine     = "Kameas Fleet refused it."
	SyncCancelled       = "Nothing was shared."
	TeamFilesNeedSignIn = "Some team files are hidden until you sign in with `kenaz login`."
	SyncPickTeam        = "Which team? Type its number or name: "
	SyncNoSuchTeam      = "That isn't one of your teams, so nothing was shared."
)

// SyncStopped says the run stopped sending because Kameas Fleet wasn't answering.
func SyncStopped(n int) string {
	verb := " weren't"
	if n == 1 {
		verb = " wasn't"
	}
	return "Stopped, because Kameas Fleet isn't answering: " + plural(n, "file") + verb + " sent. Run sync again."
}

// SyncLegacyLine reports older files nobody can be shown to own.
func SyncLegacyLine(n int, from, to string) string {
	return plural(n, "older file") + " in " + from + " won't be sent, because Kenaz can't tell whose it is. To share one, move it into " + to + "."
}

// SyncNotesLine says shared notes aren't part of the folder.
func SyncNotesLine(n int) string {
	if n == 1 {
		return "1 shared note stays on Kameas Fleet, because it isn't a file."
	}
	return plural(n, "shared note") + " stay on Kameas Fleet, because they aren't files."
}

// SyncFolderUnreadable says a folder's owner can't be read.
func SyncFolderUnreadable(folder string) string {
	return "Kenaz can't read who the folder " + folder + " belongs to, so nothing in it was sent. Delete the hidden file .kenaz-folder.json in it, and Kenaz will check the folder again."
}

// SyncFolderBound says a folder that held files is now this account's and team's.
func SyncFolderBound(folder, team string) string {
	return "The folder " + folder + " now belongs to this account and to " + team + "."
}

// SyncUnclaimed reports a folder of this account's that matches none of its teams.
func SyncUnclaimed(folder string) string {
	return "The folder " + folder + " has files, but you have no team of that name, so they aren't sent. To share them, move them into your team's folder."
}

// SyncConflictLine says both sides changed a file, and where each text is.
func SyncConflictLine(name, yours string) string {
	return name + " changed on both sides: your team's text is in " + name + ", and yours is in " + yours + "."
}

// SyncResult is the one line after sending.
func SyncResult(sent, total int, team string) string {
	if sent == total {
		return "Shared " + plural(sent, "file") + " with " + team + "."
	}
	return "Shared " + strconv.Itoa(sent) + " of " + plural(total, "file") + " with " + team + "."
}

// EraseTryingAgain follows the line saying an erase is in progress, once an attempt hasn't
// been answered. It may have worked, so it never says the erase failed.
const EraseTryingAgain = "Kameas Fleet hasn't confirmed it yet. Kenaz will try again to confirm whenever your AI tool is running."

// ImportingNotes is the progress line.
func ImportingNotes(done, total int) string {
	return "Importing notes: " + strconv.Itoa(done) + " of " + strconv.Itoa(total) + "…"
}

// RelayStillOld says another version's relay is what's still running.
func RelayStillOld(version string) string {
	return "Kenaz relay " + version + " is still running instead of this version, so notes may not send. Close your AI tools, then run this again."
}

// MemoryOnResult is the one line that says what happened.
func MemoryOnResult(done []string, imported, skipped int) string {
	line := "Memory is on: " + done[0]
	for _, d := range done[1:] {
		line += " and " + d
	}
	line += " running"
	if imported > 0 {
		line += ", " + plural(imported, "note") + " brought over"
	}
	if skipped > 0 {
		line += ", " + plural(skipped, "note") + " couldn't be brought over"
	}
	return line + "."
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

// Approving what an agent asked to write.
const (
	PageLede    = "Check the exact text below, then approve or decline."
	PageApprove = "Approve"
	PageDecline = "Decline"

	// ToolApproval is the sentence tool descriptions carry.
	ToolApproval = "Nothing is written by this call. Give the user the link and the command from the result: they check the exact text, then approve or decline."

	ApprovalUnavailable = "Nothing was written. Approvals need the Kenaz relay and this device's keychain, and one of them isn't available here."
)

// ApprovalPending is the one line a tool result gives: both ways to approve.
func ApprovalPending(url, id string) string {
	if url == "" {
		return "Nothing has been written yet. Check the exact text, then approve or decline with `kenaz approve " + id + "`."
	}
	return "Nothing has been written yet. Check the exact text, then approve or decline at " + url + " or with `kenaz approve " + id + "`."
}
