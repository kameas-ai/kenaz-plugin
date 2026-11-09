package fleetapitest

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
)

// The id goes when there is one, and the body is as before when there isn't.
func TestForgetAllBody(t *testing.T) {
	b, _ := json.Marshal(fleetapi.MemoryForgetAllRequest{Confirm: fleetapi.MemoryForgetAllConfirm, RequestID: "abc"})
	if string(b) != `{"confirm":"forget-all","request_id":"abc"}` {
		t.Fatalf("%s", b)
	}
	b, _ = json.Marshal(fleetapi.MemoryForgetAllRequest{Confirm: fleetapi.MemoryForgetAllConfirm})
	if string(b) != `{"confirm":"forget-all"}` {
		t.Fatalf("%s", b)
	}
}

// Fleet's own rules for a repeat.
func TestForgetAllRepeats(t *testing.T) {
	f := NewMemoryFleet()
	m := f.Client("org", "user")
	ctx := f.ContextFor(context.Background(), "org", "user")
	body := func(id string) fleetapi.MemoryForgetAllRequest {
		return fleetapi.MemoryForgetAllRequest{Confirm: fleetapi.MemoryForgetAllConfirm, RequestID: id}
	}
	first, err := m.ForgetAll(ctx, body("req-1"))
	if err != nil || f.Erases != 1 {
		t.Fatalf("%v, %d erases", err, f.Erases)
	}
	for _, id := range []string{"req-1", ""} {
		got, err := m.ForgetAll(ctx, body(id))
		if err != nil || got != first || f.Erases != 1 {
			t.Fatalf("%q: %+v %v, %d erases", id, got, err, f.Erases)
		}
	}
	if _, err := m.ForgetAll(ctx, body("req-2")); err != nil || f.Erases != 2 {
		t.Fatalf("a different id: %v, %d erases", err, f.Erases)
	}
	if _, err := m.ForgetAll(ctx, body("not ok")); fleetapi.APICode(err) != fleetapi.CodeInvalidRequestBody {
		t.Fatalf("%v", err)
	}
}
