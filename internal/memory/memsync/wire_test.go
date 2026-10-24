package memsync

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/allowlist"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/fleetapitest"
	"github.com/kameas-ai/kenaz-plugin/internal/memory"
)

// The store builds payloads without importing fleetapi; they must decode
// into the seam's item type without loss and carry only allowlisted keys.
func TestStorePayloadsMatchSeam(t *testing.T) {
	ctx := context.Background()
	f := fleetapitest.NewMemoryFleet()
	a := newDevice(t, f, alice)
	a.enable()
	id := a.save("Title", "first", true)
	a.sync()
	nt := "Title 2"
	a.st.UpdateNote(ctx, id, memory.Update{Title: &nt}, memory.MutateOptions{SyncApproved: true})
	a.save("Other", "body", true)
	l, _ := a.st.AcquireLease(ctx, a.holder, leaseTTL)
	b, err := a.st.LeasePushBatch(ctx, l)
	if err != nil || len(b.Ops) != 2 {
		t.Fatalf("batch = %+v %v", b, err)
	}
	items, err := toItems(b.Ops)
	if err != nil {
		t.Fatal(err)
	}
	for i, op := range b.Ops {
		again, _ := json.Marshal(items[i])
		var x, y any
		json.Unmarshal(op.Payload, &x)
		json.Unmarshal(again, &y)
		if !reflect.DeepEqual(x, y) {
			t.Fatalf("payload %s != %s", op.Payload, again)
		}
	}
	body, _ := json.Marshal(fleetapi.MemoryPushRequest{DeviceID: a.st.DeviceID(), BaseCursor: b.BaseCursor, Items: items})
	if err := allowlist.Memory().CheckPushBody(body); err != nil {
		t.Fatal(err)
	}
	if items[1].Content == nil || items[1].ContentHash != memory.ContentHash("body") || items[1].Fields["scope_kind"].HLC == "" {
		t.Fatalf("create item = %+v", items[1])
	}
	if items[0].Content != nil || string(items[0].Fields["title"].V) != `"Title 2"` || items[0].ID != id {
		t.Fatalf("title item = %+v", items[0])
	}
}
