package app

import (
	"bytes"
	"testing"
)

// `kenaz memory sync status` keeps the raw codes on the refused line and
// explains underneath the ones that have plain wording.
func TestWriteRefusedCodes(t *testing.T) {
	var b bytes.Buffer
	writeRefusedCodes(&b, nil)
	if b.Len() != 0 {
		t.Fatalf("no codes printed %q", b.String())
	}
	writeRefusedCodes(&b, []string{"created_at_in_future", "erased_before_cutoff", "some_new_code"})
	want := "refused:  created_at_in_future, erased_before_cutoff, some_new_code\n" +
		"          created_at_in_future: The note stays on this device and was not synced, because this device's clock looks ahead of Kameas Fleet's. Check this device's date and time; after that, editing the note's text saves it as a new note that can sync.\n" +
		"          erased_before_cutoff: The note stays on this device and was not synced, because it was written before, or within a few minutes of, a \"forget everything\" on your account. Editing the note's text saves it as a new note that can sync.\n"
	if b.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", b.String(), want)
	}
}
