package fleetmanager

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Gameye/nakama-fleetmanager/gameye"
	"github.com/heroiclabs/nakama-common/runtime"
)

func listing(ids ...string) func(context.Context, gameye.SessionList) ([]gameye.SessionListEntry, error) {
	return func(context.Context, gameye.SessionList) ([]gameye.SessionListEntry, error) {
		var out []gameye.SessionListEntry
		for _, id := range ids {
			out = append(out, gameye.SessionListEntry{ID: id, Status: "running"})
		}
		return out, nil
	}
}

func TestReapDeletesRowsGameyeNoLongerHas(t *testing.T) {
	fapi := newFakeApi()
	fapi.listFn = listing("present", "unstored")
	fm, store, _ := newTestFleetManager(t, testConfig(), fapi)
	store.put(t, &runtime.InstanceInfo{Id: "present"})
	store.put(t, &runtime.InstanceInfo{Id: "absent"})

	if err := fm.reap(context.Background()); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if store.stored(t, "present") == nil {
		t.Fatalf("row for a listed session was deleted")
	}
	if store.stored(t, "absent") != nil {
		t.Fatalf("row for an unlisted session was kept")
	}
	if store.stored(t, "unstored") != nil {
		t.Fatalf("the reaper must not add rows")
	}
	// Rows may belong to sessions of an earlier region, image or version, so
	// the reaper lists without filters.
	if got := fapi.lists[0]; got != (gameye.SessionList{}) {
		t.Fatalf("reaper list filter = %+v, want none", got)
	}
}

func TestReapListErrorDeletesNothing(t *testing.T) {
	fapi := newFakeApi()
	fapi.listFn = func(context.Context, gameye.SessionList) ([]gameye.SessionListEntry, error) {
		return nil, &gameye.ApiError{StatusCode: 503}
	}
	fm, store, _ := newTestFleetManager(t, testConfig(), fapi)
	store.put(t, &runtime.InstanceInfo{Id: "a"})

	if err := fm.reap(context.Background()); !errors.Is(err, gameye.ErrInternalServer) {
		t.Fatalf("err = %v", err)
	}
	if store.stored(t, "a") == nil {
		t.Fatalf("row deleted after a failed list")
	}
}

// A session stored after the reaper read storage must survive even though the
// Gameye listing (taken later) is what the reaper compares against.
func TestReapKeepsRowsStoredDuringTheCycle(t *testing.T) {
	fapi := newFakeApi()
	fm, store, _ := newTestFleetManager(t, testConfig(), fapi)
	fapi.listFn = func(context.Context, gameye.SessionList) ([]gameye.SessionListEntry, error) {
		store.put(t, &runtime.InstanceInfo{Id: "new"}) // Create finished meanwhile
		return nil, nil
	}
	store.put(t, &runtime.InstanceInfo{Id: "old"})

	if err := fm.reap(context.Background()); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if store.stored(t, "new") == nil {
		t.Fatalf("a row written during the cycle was reaped")
	}
	if store.stored(t, "old") != nil {
		t.Fatalf("row for an unlisted session was kept")
	}
}

func TestReaperRunsFromInitAndStopsWithContext(t *testing.T) {
	fapi := newFakeApi()
	fapi.listFn = listing("present")
	cfg := testConfig()
	cfg.ReapInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := newFakeStorage()
	store.put(t, &runtime.InstanceInfo{Id: "present"})
	store.put(t, &runtime.InstanceInfo{Id: "absent"})
	fm := &GameyeFleetManager{ctx: ctx, config: cfg.withDefaults(), logger: nopLogger{}, apiClient: fapi, storage: store}
	if err := fm.Init(newFakeNk(), newFakeCallbackHandler()); err != nil {
		t.Fatalf("Init: %v", err)
	}

	eventually(t, "the reaper to delete the absent row", func() bool { return store.raw("absent") == "" })
	if store.stored(t, "present") == nil {
		t.Fatalf("row for a listed session was deleted")
	}

	cancel()
	select {
	case <-fm.reaperDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("reaper did not stop when its context ended")
	}
	store.put(t, &runtime.InstanceInfo{Id: "late"})
	time.Sleep(50 * time.Millisecond)
	if store.stored(t, "late") == nil {
		t.Fatalf("reaper still running after its context ended")
	}
}

func TestReaperDisabledByNegativeInterval(t *testing.T) {
	fapi := newFakeApi()
	fm, store, _ := newTestFleetManager(t, testConfig(), fapi) // ReapInterval -1
	store.put(t, &runtime.InstanceInfo{Id: "absent"})
	time.Sleep(50 * time.Millisecond)
	if fm.reaperDone != nil || len(fapi.lists) != 0 {
		t.Fatalf("reaper started although disabled")
	}
}

func TestReapIntervalDefault(t *testing.T) {
	if got := testConfig().withDefaults().ReapInterval; got != 2*time.Minute {
		t.Fatalf("ReapInterval default = %v, want 2m", got)
	}
}

func TestInitStartsOneReaper(t *testing.T) {
	fapi := newFakeApi()
	cfg := testConfig()
	cfg.ReapInterval = time.Hour
	fm, _, _ := newTestFleetManager(t, cfg, fapi)
	first := fm.reaperDone
	if err := fm.Init(newFakeNk(), newFakeCallbackHandler()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if first == nil || fm.reaperDone != first {
		t.Fatalf("a second Init must not start another reaper")
	}
}

// Gameye keeps listing a session after it stops. Only running, draining and
// shutting-down sessions keep their rows.
func TestReapDeletesExitedRowsAndKeepsDraining(t *testing.T) {
	fapi := newFakeApi()
	fapi.listFn = func(context.Context, gameye.SessionList) ([]gameye.SessionListEntry, error) {
		return []gameye.SessionListEntry{
			{ID: "exited", Status: "exited"},
			{ID: "draining", Status: "draining"},
		}, nil
	}
	fm, store, _ := newTestFleetManager(t, testConfig(), fapi)
	store.put(t, &runtime.InstanceInfo{Id: "exited"})
	store.put(t, &runtime.InstanceInfo{Id: "draining"})

	if err := fm.reap(context.Background()); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if store.stored(t, "exited") != nil {
		t.Fatalf("row for an exited session was kept")
	}
	if store.stored(t, "draining") == nil {
		t.Fatalf("row for a draining session was deleted")
	}
}
