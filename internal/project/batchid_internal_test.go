package project

import (
	"errors"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/internal/fixtures"
	"github.com/kameas-ai/kenaz-plugin/internal/fleetschema"
)

// A random source that fails, or that reports success without producing
// random bytes, must never yield a batch: Encode panics, as it does for
// span ids, instead of emitting a weak, empty or all-zero id.
func TestBatchIDRandFailureEmitsNothing(t *testing.T) {
	recs := []Record{{
		Class: fleetschema.KindClass[fleetschema.KindError], Client: "claude-code",
		Time: fixtures.Base, EventKind: fleetschema.KindError,
	}}
	id := Identity{UserID: "u", OrgID: "o", MachineID: "m"}
	for name, read := range map[string]func([]byte) (int, error){
		"error": func([]byte) (int, error) { return 0, errors.New("no entropy") },
		"zeros": func(b []byte) (int, error) { return len(b), nil },
	} {
		t.Run(name, func(t *testing.T) {
			old := randRead
			randRead = read
			defer func() { randRead = old }()
			defer func() {
				if recover() == nil {
					t.Fatal("Encode returned with a broken random source")
				}
			}()
			bs, err := Encode(recs, id, Options{})
			t.Fatalf("Encode = %d batches, err %v", len(bs), err)
		})
	}
}
