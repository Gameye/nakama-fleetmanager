package fleetmanager

import (
	"context"
	"fmt"
	"testing"

	"github.com/heroiclabs/nakama-common/runtime"
)

func TestNakamaStorageRoundTrip(t *testing.T) {
	nk := newFakeNk()
	s := NewNakamaStorage(nk)
	ctx := context.Background()

	got, err := s.Read(ctx, "missing")
	if err != nil || got != nil {
		t.Fatalf("Read(missing) = %+v, %v; want nil, nil", got, err)
	}

	in := &runtime.InstanceInfo{Id: "a", Status: "running", PlayerCount: 2, ConnectionInfo: &runtime.ConnectionInfo{IpAddress: "1.2.3.4", Port: 21000}}
	if err := s.Write(ctx, []*runtime.InstanceInfo{in}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if w := nk.writes[0]; w.Collection != StorageGameyeInstancesCollection || w.Key != "a" || w.UserID != "" {
		t.Fatalf("write = %+v, want a system-owned row in %q", w, StorageGameyeInstancesCollection)
	}

	got, err = s.Read(ctx, "a")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.PlayerCount != 2 || got.ConnectionInfo.Port != 21000 {
		t.Fatalf("Read = %+v", got)
	}

	if err := s.Delete(ctx, []string{"a"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got, _ := s.Read(ctx, "a"); got != nil {
		t.Fatalf("row survived Delete")
	}
}

func TestNakamaStorageWriteNothingIsNoop(t *testing.T) {
	nk := newFakeNk()
	if err := NewNakamaStorage(nk).Write(context.Background(), nil); err != nil {
		t.Fatalf("Write(nil): %v", err)
	}
	if err := NewNakamaStorage(nk).Delete(context.Background(), nil); err != nil {
		t.Fatalf("Delete(nil): %v", err)
	}
	if len(nk.writes) != 0 {
		t.Fatalf("writes = %v", nk.writes)
	}
}

func TestNakamaStorageListIdsPages(t *testing.T) {
	nk := newFakeNk()
	nk.pageSize = 2
	s := NewNakamaStorage(nk)
	var want []string
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("id-%d", i)
		want = append(want, id)
		if err := s.Write(context.Background(), []*runtime.InstanceInfo{{Id: id}}); err != nil {
			t.Fatal(err)
		}
	}
	// A row from another collection must not be listed.
	nk.StorageWrite(context.Background(), []*runtime.StorageWrite{{Collection: "other", Key: "x", Value: "{}"}})

	ids, err := s.ListIds(context.Background())
	if err != nil {
		t.Fatalf("ListIds: %v", err)
	}
	if fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	if len(nk.lists) < 3 {
		t.Fatalf("expected paging over %d calls, got %d", 3, len(nk.lists))
	}
}
