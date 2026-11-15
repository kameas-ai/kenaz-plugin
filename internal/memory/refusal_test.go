package memory

import "testing"

// Fleet's final refusals with plain wording say what happened and what to
// do, and keep the code alongside so support can match it. A code without
// wording still shows raw.
func TestRefusalWording(t *testing.T) {
	const (
		erased = `The note stays on this device and was not synced, because it was written before, or within a few minutes of, a "forget everything" on your account. Editing the note's text saves it as a new note that can sync.`
		clock  = `The note stays on this device and was not synced, because this device's clock looks ahead of Kameas Fleet's. Check this device's date and time; after that, editing the note's text saves it as a new note that can sync.`
	)
	for code, want := range map[string]string{
		"erased_before_cutoff": erased,
		"created_at_in_future": clock,
		"clock_in_future":      clock,
		"some_new_code":        "",
		"":                     "",
	} {
		if got := RefusalExplanation(code); got != want {
			t.Errorf("RefusalExplanation(%q) = %q, want %q", code, got, want)
		}
	}
	for code, want := range map[string]string{
		"erased_before_cutoff": erased + " (erased_before_cutoff)",
		"created_at_in_future": clock + " (created_at_in_future)",
		"clock_in_future":      clock + " (clock_in_future)",
		"some_new_code":        "Saved on this device only: Kameas Fleet refused it (some_new_code).",
		"secret_detected":      "Saved on this device only: it looks like it contains a credential, so it will not be synced.",
	} {
		if got := StateExplanation(StateRejected, code); got != want {
			t.Errorf("StateExplanation(rejected, %q) = %q, want %q", code, got, want)
		}
	}
	// The wording is for a final refusal only: the same code on a note in
	// another state changes nothing.
	if got := StateExplanation(StatePending, "erased_before_cutoff"); got != "Saved on this device; queued to sync to your Kameas Fleet account." {
		t.Errorf("pending = %q", got)
	}
}
