package fleetmanager

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Gameye/nakama-fleetmanager/gameye"
	gameyeApi "github.com/Gameye/nakama-fleetmanager/pkg/api/generated/openapi/client"
	"github.com/heroiclabs/nakama-common/runtime"
)

// --- Create ----------------------------------------------------------------

func TestCreateReturnsSessionId(t *testing.T) {
	fapi := newFakeApi()
	fm, _, handler := newTestFleetManager(t, testConfig(), fapi)

	out, err := fm.Create(context.Background(), 2, nil, nil, nil, func(runtime.FmCreateStatus, *runtime.InstanceInfo, []*runtime.SessionInfo, map[string]any, error) {})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if out[CreateSessionIdKey] == "" {
		t.Fatalf("Create returned %v, want a %q entry", out, CreateSessionIdKey)
	}

	res := handler.wait(t)
	if res.status != runtime.CreateSuccess {
		t.Fatalf("status = %v, err = %v", res.status, res.err)
	}
	if res.instance.Id != out[CreateSessionIdKey] {
		t.Fatalf("instance id %q != returned id %q", res.instance.Id, out[CreateSessionIdKey])
	}
	if got := fapi.lastRun(t).ID; got != out[CreateSessionIdKey] {
		t.Fatalf("requested id %q != returned id %q", got, out[CreateSessionIdKey])
	}
}

func TestCreateSurvivesCallerContextCancellation(t *testing.T) {
	release := make(chan struct{})
	fapi := newFakeApi()
	fapi.runFn = func(ctx context.Context, req gameye.SessionRun) (*gameye.SessionStarted, error) {
		<-release
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return &gameye.SessionStarted{ID: req.ID, Host: "1.2.3.4", Ports: []gameye.Port{{Type: "tcp", Container: 7360, Host: 21000}}}, nil
	}
	fm, store, handler := newTestFleetManager(t, testConfig(), fapi)

	// Nakama 3.39+ cancels the MatchmakerMatched context when the hook returns.
	ctx, cancel := context.WithCancel(context.Background())
	out, err := fm.Create(ctx, 2, nil, nil, nil, nil)
	cancel()
	close(release)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	res := handler.wait(t)
	if res.status != runtime.CreateSuccess {
		t.Fatalf("status = %v, err = %v; want CreateSuccess", res.status, res.err)
	}
	if store.stored(t, out[CreateSessionIdKey]) == nil {
		t.Fatalf("instance was not written to storage (ctx errors: %v)", store.ctxErrs)
	}
}

func TestCreateTimesOut(t *testing.T) {
	fapi := newFakeApi()
	fapi.runFn = func(ctx context.Context, req gameye.SessionRun) (*gameye.SessionStarted, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	cfg := testConfig()
	cfg.CreateTimeout = 50 * time.Millisecond
	fm, store, handler := newTestFleetManager(t, cfg, fapi)

	start := time.Now()
	out, err := fm.Create(context.Background(), 2, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	res := handler.wait(t)
	if res.status != runtime.CreateTimeout {
		t.Fatalf("status = %v, err = %v; want CreateTimeout", res.status, res.err)
	}
	if res.instance != nil {
		t.Fatalf("instance must be nil on timeout")
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("timed out after %v, before the configured timeout", elapsed)
	}
	// The session might still start on Gameye's side; it is stopped best-effort.
	select {
	case id := <-fapi.stopped:
		if id != out[CreateSessionIdKey] {
			t.Fatalf("stopped %q, want %q", id, out[CreateSessionIdKey])
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed-out session was not stopped")
	}
	if store.stored(t, out[CreateSessionIdKey]) != nil {
		t.Fatalf("timed-out session must not be stored")
	}
}

func TestCreateUsesRequestedIdWhenResponseHasNone(t *testing.T) {
	fapi := newFakeApi()
	fapi.runFn = func(ctx context.Context, req gameye.SessionRun) (*gameye.SessionStarted, error) {
		return &gameye.SessionStarted{Host: "1.2.3.4", Ports: []gameye.Port{{Type: "tcp", Container: 7360, Host: 21000}}}, nil
	}
	fm, store, handler := newTestFleetManager(t, testConfig(), fapi)

	out, err := fm.Create(context.Background(), 2, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	res := handler.wait(t)
	if res.status != runtime.CreateSuccess {
		t.Fatalf("status = %v, err = %v", res.status, res.err)
	}
	if res.instance.Id != out[CreateSessionIdKey] {
		t.Fatalf("instance id = %q, want requested %q", res.instance.Id, out[CreateSessionIdKey])
	}
	if store.stored(t, out[CreateSessionIdKey]) == nil {
		t.Fatalf("instance not stored under the requested id")
	}
}

func TestCreateSendsTtlExternalIdAndVersion(t *testing.T) {
	fapi := newFakeApi()
	fm, _, handler := newTestFleetManager(t, testConfig(), fapi)

	_, err := fm.Create(context.Background(), 2, nil, nil, map[string]any{
		MetadataKeyExternalId: "match-42",
		"mode":                "duel",
	}, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	handler.wait(t)

	run := fapi.lastRun(t)
	if run.Ttl != DefaultTtl {
		t.Errorf("Ttl = %q, want default %q", run.Ttl, DefaultTtl)
	}
	if run.ExternalID != "match-42" {
		t.Errorf("ExternalID = %q", run.ExternalID)
	}
	if run.Tag != "1.0.0" || run.Region != "europe" || run.Image != "my-game" {
		t.Errorf("unexpected run %+v", run)
	}
	if run.Labels["mode"] != "duel" {
		t.Errorf("labels = %v", run.Labels)
	}
	if _, ok := run.Labels[MetadataKeyExternalId]; ok {
		t.Errorf("reserved key leaked into labels: %v", run.Labels)
	}
}

func TestCreateConfiguredTtl(t *testing.T) {
	fapi := newFakeApi()
	cfg := testConfig()
	cfg.Ttl = "2h"
	fm, _, handler := newTestFleetManager(t, cfg, fapi)
	if _, err := fm.Create(context.Background(), 2, nil, nil, nil, nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	handler.wait(t)
	if got := fapi.lastRun(t).Ttl; got != "2h" {
		t.Fatalf("Ttl = %q, want 2h", got)
	}
}

func TestCreateRejectsNonStringExternalId(t *testing.T) {
	fapi := newFakeApi()
	fm, _, _ := newTestFleetManager(t, testConfig(), fapi)
	if _, err := fm.Create(context.Background(), 2, nil, nil, map[string]any{MetadataKeyExternalId: 7}, nil); err == nil {
		t.Fatalf("expected an error for a non-string external id")
	}
	if len(fapi.runs) != 0 {
		t.Fatalf("no API call expected")
	}
}

func TestCreateSelectsConfiguredPort(t *testing.T) {
	fapi := newFakeApi()
	fapi.runFn = func(ctx context.Context, req gameye.SessionRun) (*gameye.SessionStarted, error) {
		return &gameye.SessionStarted{ID: req.ID, Host: "1.2.3.4", Ports: []gameye.Port{
			{Type: "tcp", Container: 9000, Host: 20001},
			{Type: "tcp", Container: 7360, Host: 20002},
		}}, nil
	}
	cfg := testConfig()
	cfg.Port = "7360/tcp"
	fm, _, handler := newTestFleetManager(t, cfg, fapi)

	if _, err := fm.Create(context.Background(), 2, nil, nil, nil, nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	res := handler.wait(t)
	if res.status != runtime.CreateSuccess {
		t.Fatalf("status = %v, err = %v", res.status, res.err)
	}
	if res.instance.ConnectionInfo.Port != 20002 || res.instance.ConnectionInfo.IpAddress != "1.2.3.4" {
		t.Fatalf("connection info = %+v, want 1.2.3.4:20002", res.instance.ConnectionInfo)
	}
}

func TestCreateMissingConfiguredPortStopsSession(t *testing.T) {
	fapi := newFakeApi()
	cfg := testConfig()
	cfg.Port = "7777/udp"
	fm, store, handler := newTestFleetManager(t, cfg, fapi)

	out, err := fm.Create(context.Background(), 2, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	res := handler.wait(t)
	if res.status != runtime.CreateError || res.err == nil {
		t.Fatalf("status = %v, err = %v; want CreateError", res.status, res.err)
	}
	select {
	case id := <-fapi.stopped:
		if id != out[CreateSessionIdKey] {
			t.Fatalf("stopped %q", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("session without the configured port was not stopped")
	}
	if store.stored(t, out[CreateSessionIdKey]) != nil {
		t.Fatalf("session must not be stored")
	}
}

func TestCreateApiErrorIsCreateErrorWithSentinel(t *testing.T) {
	fapi := newFakeApi()
	fapi.runFn = func(ctx context.Context, req gameye.SessionRun) (*gameye.SessionStarted, error) {
		return nil, &gameye.ApiError{StatusCode: 420}
	}
	fm, _, handler := newTestFleetManager(t, testConfig(), fapi)

	if _, err := fm.Create(context.Background(), 2, nil, nil, nil, nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	res := handler.wait(t)
	if res.status != runtime.CreateError || !errors.Is(res.err, gameye.ErrNoCapacity) {
		t.Fatalf("status = %v, err = %v; want CreateError wrapping ErrNoCapacity", res.status, res.err)
	}
}

// --- Get / List port selection --------------------------------------------

func TestGetAndListSelectConfiguredPort(t *testing.T) {
	ports := map[string]int{"9000/tcp": 20001, "7360/tcp": 20002, "7360/udp": 20003}
	fapi := newFakeApi()
	fapi.descFn = func(ctx context.Context, req gameye.SessionDescribe) (*gameye.Session, error) {
		return &gameye.Session{ID: req.ID, Status: "running", IPV4Address: "1.2.3.4", Ports: ports}, nil
	}
	fapi.listFn = func(ctx context.Context, req gameye.SessionList) ([]gameye.SessionListEntry, error) {
		return []gameye.SessionListEntry{{ID: "a", Status: "running", IPV4Address: "1.2.3.4", Ports: ports}}, nil
	}
	cfg := testConfig()
	cfg.Port = "7360/udp"
	fm, _, _ := newTestFleetManager(t, cfg, fapi)

	for i := 0; i < 20; i++ {
		inst, err := fm.Get(context.Background(), "a")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if inst.ConnectionInfo.Port != 20003 {
			t.Fatalf("Get port = %d, want 20003", inst.ConnectionInfo.Port)
		}
		list, _, err := fm.List(context.Background(), "", 0, "")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if list[0].ConnectionInfo.Port != 20003 {
			t.Fatalf("List port = %d, want 20003", list[0].ConnectionInfo.Port)
		}
	}
}

// --- config ----------------------------------------------------------------

func TestConfigDefaultsAndValidation(t *testing.T) {
	cfg := GameyeConfig{ApiToken: "t", Region: "r", Image: "i", Version: "v"}.withDefaults()
	if cfg.BaseUrl != gameye.DefaultBaseUrl {
		t.Errorf("BaseUrl default = %q", cfg.BaseUrl)
	}
	if cfg.Ttl != DefaultTtl {
		t.Errorf("Ttl default = %q", cfg.Ttl)
	}
	if cfg.CreateTimeout != DefaultCreateTimeout {
		t.Errorf("CreateTimeout default = %v", cfg.CreateTimeout)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}

	bad := cfg
	bad.Port = "7360"
	if err := bad.Validate(); !errors.Is(err, ErrInvalidPort) {
		t.Errorf("port without protocol: err = %v", err)
	}
	bad = cfg
	bad.Ttl = "30s"
	if err := bad.Validate(); !errors.Is(err, ErrInvalidTtl) {
		t.Errorf("ttl in seconds: err = %v", err)
	}
	for _, ok := range []string{"30m", "1h", "2h30m"} {
		good := cfg
		good.Ttl = ok
		if err := good.Validate(); err != nil {
			t.Errorf("ttl %q rejected: %v", ok, err)
		}
	}
}

func TestNewGameyeFleetManagerAppliesDefaults(t *testing.T) {
	cfg := testConfig()
	cfg.BaseUrl = ""
	fm, err := NewGameyeFleetManager(context.Background(), cfg, nopLogger{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewGameyeFleetManager: %v", err)
	}
	if fm.config.BaseUrl != gameye.DefaultBaseUrl {
		t.Errorf("BaseUrl = %q, want %q", fm.config.BaseUrl, gameye.DefaultBaseUrl)
	}
	if fm.config.Ttl != DefaultTtl {
		t.Errorf("Ttl = %q, want %q", fm.config.Ttl, DefaultTtl)
	}
	if fm.config.CreateTimeout != DefaultCreateTimeout {
		t.Errorf("CreateTimeout = %v, want %v", fm.config.CreateTimeout, DefaultCreateTimeout)
	}
	if fm.config.ReapInterval != DefaultReapInterval {
		t.Errorf("ReapInterval = %v, want %v", fm.config.ReapInterval, DefaultReapInterval)
	}
	if fm.apiClient == nil {
		t.Errorf("apiClient not set")
	}
}

func TestNewGameyeFleetManagerJoinsValidationErrors(t *testing.T) {
	cfg := GameyeConfig{Port: "7360", Ttl: "30s", CreateTimeout: -time.Second}
	fm, err := NewGameyeFleetManager(context.Background(), cfg, nopLogger{}, nil, nil, nil)
	if fm != nil {
		t.Fatalf("got a fleet manager for an invalid config")
	}
	for _, want := range []error{ErrNoApiToken, ErrNoRegion, ErrNoImage, ErrNoVersion, ErrInvalidPort, ErrInvalidTtl, ErrInvalidTimeout} {
		if !errors.Is(err, want) {
			t.Errorf("err does not wrap %v: %v", want, err)
		}
	}
	// An empty BaseUrl gets the default, so it is not an error.
	if errors.Is(err, ErrNoBaseUrl) {
		t.Errorf("empty BaseUrl reported as missing: %v", err)
	}
}

// --- Get -------------------------------------------------------------------

func describeAs(status gameyeApi.SessionStatus) func(ctx context.Context, req gameye.SessionDescribe) (*gameye.Session, error) {
	return func(ctx context.Context, req gameye.SessionDescribe) (*gameye.Session, error) {
		return &gameye.Session{
			ID:          req.ID,
			Created:     time.UnixMilli(1_700_000_000_000),
			PlayerCount: 3,
			Status:      status,
			IPV4Address: "1.2.3.4",
			Ports:       map[string]int{"7360/tcp": 21000},
		}, nil
	}
}

func TestGetRunningSessionWritesStorage(t *testing.T) {
	for _, status := range []gameyeApi.SessionStatus{gameyeApi.Running, gameyeApi.Draining, gameyeApi.Shuttingdown} {
		t.Run(string(status), func(t *testing.T) {
			fapi := newFakeApi()
			fapi.descFn = describeAs(status)
			fm, store, _ := newTestFleetManager(t, testConfig(), fapi)

			inst, err := fm.Get(context.Background(), "a")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if inst.Id != "a" || inst.PlayerCount != 3 || inst.Status != string(status) ||
				inst.ConnectionInfo.IpAddress != "1.2.3.4" || inst.ConnectionInfo.Port != 21000 {
				t.Fatalf("unexpected instance %+v %+v", inst, inst.ConnectionInfo)
			}
			got := store.stored(t, "a")
			if got == nil || got.PlayerCount != 3 || got.Status != string(status) {
				t.Fatalf("stored = %+v, want the described instance", got)
			}
		})
	}
}

func TestGetStoppedSessionDeletesStorage(t *testing.T) {
	for _, status := range []gameyeApi.SessionStatus{gameyeApi.Exited, gameyeApi.Dead} {
		t.Run(string(status), func(t *testing.T) {
			fapi := newFakeApi()
			fapi.descFn = describeAs(status)
			fm, store, _ := newTestFleetManager(t, testConfig(), fapi)
			store.put(t, &runtime.InstanceInfo{Id: "a", Status: "running"})

			inst, err := fm.Get(context.Background(), "a")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if inst.Status != string(status) {
				t.Fatalf("status = %q", inst.Status)
			}
			if store.stored(t, "a") != nil {
				t.Fatalf("stopped session must be removed from storage")
			}
		})
	}
}

func TestGetUnknownSessionDeletesStorageAndWrapsNotFound(t *testing.T) {
	fapi := newFakeApi()
	fapi.descFn = func(ctx context.Context, req gameye.SessionDescribe) (*gameye.Session, error) {
		return nil, &gameye.ApiError{StatusCode: 404}
	}
	fm, store, _ := newTestFleetManager(t, testConfig(), fapi)
	store.put(t, &runtime.InstanceInfo{Id: "gone", Status: "running"})

	_, err := fm.Get(context.Background(), "gone")
	if !errors.Is(err, gameye.ErrNotFound) {
		t.Fatalf("err = %v, want one wrapping gameye.ErrNotFound", err)
	}
	if store.stored(t, "gone") != nil {
		t.Fatalf("a session Gameye no longer has must be removed from storage")
	}
}

func TestGetOtherApiErrorKeepsStorage(t *testing.T) {
	fapi := newFakeApi()
	fapi.descFn = func(ctx context.Context, req gameye.SessionDescribe) (*gameye.Session, error) {
		return nil, &gameye.ApiError{StatusCode: 503}
	}
	fm, store, _ := newTestFleetManager(t, testConfig(), fapi)
	store.put(t, &runtime.InstanceInfo{Id: "a", Status: "running"})

	if _, err := fm.Get(context.Background(), "a"); !errors.Is(err, gameye.ErrInternalServer) {
		t.Fatalf("err = %v, want one wrapping ErrInternalServer", err)
	}
	if store.stored(t, "a") == nil {
		t.Fatalf("a transient error must not drop the stored instance")
	}
}

// --- Delete ----------------------------------------------------------------

func TestDeleteStopsSessionAndRemovesStorage(t *testing.T) {
	fapi := newFakeApi()
	fm, store, _ := newTestFleetManager(t, testConfig(), fapi)
	store.put(t, &runtime.InstanceInfo{Id: "a", Status: "running"})

	if err := fm.Delete(context.Background(), "a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := fapi.stopCalls(); len(got) != 1 || got[0] != "a" {
		t.Fatalf("stops = %v, want [a]", got)
	}
	if store.stored(t, "a") != nil {
		t.Fatalf("storage row not removed")
	}
}

// The ApiClient reports an already-stopped session (404/409) as success, so
// Delete still clears storage.
func TestDeleteAlreadyGoneSucceeds(t *testing.T) {
	fapi := newFakeApi()
	fm, store, _ := newTestFleetManager(t, testConfig(), fapi)

	if err := fm.Delete(context.Background(), "never-stored"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(store.deleted) != 1 || store.deleted[0] != "never-stored" {
		t.Fatalf("deleted = %v", store.deleted)
	}
}

func TestDeleteStopErrorKeepsStorage(t *testing.T) {
	fapi := newFakeApi()
	fapi.stopErr = &gameye.ApiError{StatusCode: 403}
	fm, store, _ := newTestFleetManager(t, testConfig(), fapi)
	store.put(t, &runtime.InstanceInfo{Id: "a", Status: "running"})

	if err := fm.Delete(context.Background(), "a"); !errors.Is(err, gameye.ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	if store.stored(t, "a") == nil {
		t.Fatalf("storage must be kept when the session could not be stopped")
	}
}

// --- Join ------------------------------------------------------------------

func TestJoinUsesStoredInstance(t *testing.T) {
	fapi := newFakeApi()
	fapi.descFn = func(ctx context.Context, req gameye.SessionDescribe) (*gameye.Session, error) {
		t.Errorf("Join must not describe a stored session")
		return nil, errors.New("unexpected")
	}
	fm, store, _ := newTestFleetManager(t, testConfig(), fapi)
	store.put(t, &runtime.InstanceInfo{Id: "a", Status: "running", ConnectionInfo: &runtime.ConnectionInfo{IpAddress: "1.2.3.4", Port: 21000}})

	info, err := fm.Join(context.Background(), "a", []string{"u1", "u2"}, nil)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	joins := fapi.joinCalls()
	if len(joins) != 1 || joins[0].ID != "a" || len(joins[0].PlayerIDs) != 2 {
		t.Fatalf("joins = %+v", joins)
	}
	if info.InstanceInfo.Id != "a" || info.InstanceInfo.PlayerCount != 2 || info.InstanceInfo.ConnectionInfo.Port != 21000 {
		t.Fatalf("instance = %+v", info.InstanceInfo)
	}
	if len(info.SessionInfo) != 2 || info.SessionInfo[0].UserId != "u1" || info.SessionInfo[1].UserId != "u2" || info.SessionInfo[0].SessionId != "a" {
		t.Fatalf("session info = %+v %+v", info.SessionInfo[0], info.SessionInfo[1])
	}
	if got := store.stored(t, "a"); got.PlayerCount != 2 {
		t.Fatalf("stored player count = %d, want 2", got.PlayerCount)
	}
}

func TestJoinFallsBackToGet(t *testing.T) {
	fapi := newFakeApi()
	fapi.descFn = describeAs(gameyeApi.Running)
	fapi.joinFn = func(ctx context.Context, req gameye.SessionJoin) ([]string, error) {
		return []string{"u0", "u1", "u2", "u3"}, nil // players already in the session plus the new one
	}
	fm, store, _ := newTestFleetManager(t, testConfig(), fapi)

	info, err := fm.Join(context.Background(), "a", []string{"u3"}, nil)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if len(fapi.describes) != 1 {
		t.Fatalf("describes = %v, want one fallback Get", fapi.describes)
	}
	if info.InstanceInfo.PlayerCount != 4 {
		t.Fatalf("player count = %d, want 4", info.InstanceInfo.PlayerCount)
	}
	if got := store.stored(t, "a"); got == nil || got.PlayerCount != 4 {
		t.Fatalf("stored = %+v, want player count 4", got)
	}
}

func TestJoinApiErrorIsWrapped(t *testing.T) {
	fapi := newFakeApi()
	fapi.joinFn = func(ctx context.Context, req gameye.SessionJoin) ([]string, error) {
		return nil, &gameye.ApiError{StatusCode: 404}
	}
	fm, store, _ := newTestFleetManager(t, testConfig(), fapi)
	store.put(t, &runtime.InstanceInfo{Id: "a", Status: "running", PlayerCount: 1})

	if _, err := fm.Join(context.Background(), "a", []string{"u1"}, nil); !errors.Is(err, gameye.ErrNotFound) {
		t.Fatalf("err = %v, want one wrapping ErrNotFound", err)
	}
	if got := store.stored(t, "a"); got.PlayerCount != 1 {
		t.Fatalf("player count changed on a failed join: %d", got.PlayerCount)
	}
}

// --- List ------------------------------------------------------------------

func TestListFiltersAndRefreshesStorage(t *testing.T) {
	fapi := newFakeApi()
	fapi.listFn = func(ctx context.Context, req gameye.SessionList) ([]gameye.SessionListEntry, error) {
		return []gameye.SessionListEntry{
			{ID: "a", Status: "running", PlayerCount: 2, IPV4Address: "1.2.3.4", Ports: map[string]int{"7360/tcp": 21000}},
			{ID: "b", Status: "draining", PlayerCount: 0, IPV4Address: "5.6.7.8", Ports: map[string]int{"7360/tcp": 21001}},
			{ID: "c", Status: "exited", IPV4Address: "5.6.7.8", Ports: map[string]int{"7360/tcp": 21002}},
		}, nil
	}
	fm, store, _ := newTestFleetManager(t, testConfig(), fapi)
	store.put(t, &runtime.InstanceInfo{Id: "a", PlayerCount: 0, Status: "running"})
	store.put(t, &runtime.InstanceInfo{Id: "c", Status: "running"})

	list, cursor, err := fm.List(context.Background(), "", 10, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if cursor != "" {
		t.Fatalf("cursor = %q, want none", cursor)
	}
	if len(fapi.lists) != 1 {
		t.Fatalf("lists = %v", fapi.lists)
	}
	if got := fapi.lists[0]; got.Region != "europe" || got.Image != "my-game" || got.Tag != "1.0.0" {
		t.Fatalf("list filter = %+v, want the configured region, image and tag", got)
	}
	if len(list) != 3 || list[0].Id != "a" || list[0].ConnectionInfo.Port != 21000 || list[0].PlayerCount != 2 {
		t.Fatalf("list = %+v", list)
	}
	if got := store.stored(t, "a"); got == nil || got.PlayerCount != 2 {
		t.Fatalf("stored a = %+v, want refreshed player count 2", got)
	}
	if store.stored(t, "b") == nil {
		t.Fatalf("draining session b must be stored")
	}
	if store.stored(t, "c") != nil {
		t.Fatalf("exited session c must be removed from storage")
	}
}

func TestListApiErrorLeavesStorage(t *testing.T) {
	fapi := newFakeApi()
	fapi.listFn = func(ctx context.Context, req gameye.SessionList) ([]gameye.SessionListEntry, error) {
		return nil, &gameye.ApiError{StatusCode: 403}
	}
	fm, store, _ := newTestFleetManager(t, testConfig(), fapi)
	store.put(t, &runtime.InstanceInfo{Id: "a", Status: "running"})

	if _, _, err := fm.List(context.Background(), "", 10, ""); !errors.Is(err, gameye.ErrForbidden) {
		t.Fatalf("err = %v, want one wrapping ErrForbidden", err)
	}
	if store.stored(t, "a") == nil {
		t.Fatalf("storage changed on a failed list")
	}
}

// --- Update ----------------------------------------------------------------

func TestUpdateSetsStoredPlayerCount(t *testing.T) {
	fapi := newFakeApi()
	fm, store, _ := newTestFleetManager(t, testConfig(), fapi)
	store.put(t, &runtime.InstanceInfo{Id: "a", Status: "running", PlayerCount: 1})

	if err := fm.Update(context.Background(), "a", 5, nil); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := store.stored(t, "a"); got.PlayerCount != 5 {
		t.Fatalf("player count = %d, want 5", got.PlayerCount)
	}
}

func TestUpdateUnknownFallsBackToGet(t *testing.T) {
	fapi := newFakeApi()
	fapi.descFn = describeAs(gameyeApi.Running)
	fm, store, _ := newTestFleetManager(t, testConfig(), fapi)

	if err := fm.Update(context.Background(), "a", 5, nil); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := store.stored(t, "a"); got == nil || got.PlayerCount != 3 {
		t.Fatalf("stored = %+v, want the described instance", got)
	}
}

// --- env pass-through (U3) -------------------------------------------------

const secret = "s3cr3t-seat-value"

func assertNoSecret(t *testing.T, where, s string) {
	t.Helper()
	if strings.Contains(s, secret) || strings.Contains(s, "SEAT_SECRET") {
		t.Fatalf("env leaked into %s: %s", where, s)
	}
}

func TestCreateDivertsEnvFromMetadata(t *testing.T) {
	for name, env := range map[string]any{
		"map[string]string": map[string]string{"SEAT_SECRET": secret},
		"map[string]any":    map[string]any{"SEAT_SECRET": secret},
	} {
		t.Run(name, func(t *testing.T) {
			fapi := newFakeApi()
			fm, store, handler := newTestFleetManager(t, testConfig(), fapi)

			out, err := fm.Create(context.Background(), 2, nil, nil, map[string]any{
				MetadataKeyEnv: env,
				"mode":         "duel",
			}, nil)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			res := handler.wait(t)
			if res.status != runtime.CreateSuccess {
				t.Fatalf("status = %v, err = %v", res.status, res.err)
			}

			run := fapi.lastRun(t)
			if run.EnvVars["SEAT_SECRET"] != secret {
				t.Fatalf("env = %v, want SEAT_SECRET", run.EnvVars)
			}
			labels, _ := json.Marshal(run.Labels)
			assertNoSecret(t, "labels", string(labels))
			if run.Labels["mode"] != "duel" {
				t.Fatalf("labels = %v", run.Labels)
			}
			assertNoSecret(t, "storage", store.raw(out[CreateSessionIdKey]))
			if store.raw(out[CreateSessionIdKey]) == "" {
				t.Fatalf("instance not stored")
			}
			meta, _ := json.Marshal(res.instance.Metadata)
			assertNoSecret(t, "instance metadata", string(meta))
		})
	}
}

func TestCreateMergesConfigEnvMetadataWins(t *testing.T) {
	fapi := newFakeApi()
	cfg := testConfig()
	cfg.Env = map[string]string{"REGION_NAME": "eu", "MODE": "static"}
	fm, _, handler := newTestFleetManager(t, cfg, fapi)

	if _, err := fm.Create(context.Background(), 2, nil, nil, map[string]any{
		MetadataKeyEnv: map[string]string{"MODE": "ranked", "SEAT_SECRET": secret},
	}, nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	handler.wait(t)

	env := fapi.lastRun(t).EnvVars
	if env["REGION_NAME"] != "eu" || env["MODE"] != "ranked" || env["SEAT_SECRET"] != secret || len(env) != 3 {
		t.Fatalf("env = %v", env)
	}
	if cfg.Env["MODE"] != "static" || len(cfg.Env) != 2 {
		t.Fatalf("config env was mutated: %v", cfg.Env)
	}

	// Config env alone still reaches the container.
	if _, err := fm.Create(context.Background(), 2, nil, nil, nil, nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	handler.wait(t)
	if env := fapi.lastRun(t).EnvVars; env["MODE"] != "static" || len(env) != 2 {
		t.Fatalf("env = %v, want the config env", env)
	}
}

func TestCreateRejectsInvalidEnvBeforeApiCall(t *testing.T) {
	for name, env := range map[string]any{
		"non-string value": map[string]any{"SEAT_SECRET": 7},
		"nested map":       map[string]any{"A": map[string]string{"B": "c"}},
		"not a map":        "SEAT_SECRET=x",
		"empty value":      map[string]string{"SEAT_SECRET": ""},
		"empty key":        map[string]string{"": "x"},
	} {
		t.Run(name, func(t *testing.T) {
			fapi := newFakeApi()
			fm, _, _ := newTestFleetManager(t, testConfig(), fapi)
			if _, err := fm.Create(context.Background(), 2, nil, nil, map[string]any{MetadataKeyEnv: env}, nil); err == nil {
				t.Fatalf("expected an error")
			}
			if fapi.runCount() != 0 {
				t.Fatalf("no API call expected")
			}
		})
	}
}

// A plain "env" metadata key would become a label; Gameye can echo env as the
// "env" label, so the key is reserved.
func TestCreateRejectsEnvLabel(t *testing.T) {
	fapi := newFakeApi()
	fm, _, _ := newTestFleetManager(t, testConfig(), fapi)
	if _, err := fm.Create(context.Background(), 2, nil, nil, map[string]any{"env": "x"}, nil); err == nil {
		t.Fatalf("expected an error for metadata key \"env\"")
	}
	if fapi.runCount() != 0 {
		t.Fatalf("no API call expected")
	}
}

func TestCreateRejectsEmptyConfigEnvValue(t *testing.T) {
	cfg := testConfig()
	cfg.Env = map[string]string{"A": ""}
	if err := cfg.Validate(); !errors.Is(err, ErrInvalidEnv) {
		t.Fatalf("Validate = %v, want ErrInvalidEnv", err)
	}
}

func TestCreateSetsInstanceMetadata(t *testing.T) {
	fapi := newFakeApi()
	fm, store, handler := newTestFleetManager(t, testConfig(), fapi)

	out, err := fm.Create(context.Background(), 2, nil, nil, map[string]any{
		MetadataKeyExternalId: "match-42",
		MetadataKeyEnv:        map[string]string{"SEAT_SECRET": secret},
		"mode":                "duel",
		"max":                 4,
	}, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	res := handler.wait(t)
	if res.instance.Metadata["mode"] != "duel" || res.instance.Metadata["max"] != 4 || len(res.instance.Metadata) != 2 {
		t.Fatalf("metadata = %v, want only the non-reserved keys", res.instance.Metadata)
	}
	if got := store.stored(t, out[CreateSessionIdKey]); got.Metadata["mode"] != "duel" {
		t.Fatalf("stored metadata = %v", got.Metadata)
	}
}

func TestGetAndListStripEchoedEnvLabel(t *testing.T) {
	labels := map[string]string{"env": `{"SEAT_SECRET":"` + secret + `"}`, "mode": "duel"}
	fapi := newFakeApi()
	fapi.descFn = func(ctx context.Context, req gameye.SessionDescribe) (*gameye.Session, error) {
		return &gameye.Session{ID: req.ID, Status: gameyeApi.Running, Ports: map[string]int{"7360/tcp": 1}, Labels: labels}, nil
	}
	fapi.listFn = func(ctx context.Context, req gameye.SessionList) ([]gameye.SessionListEntry, error) {
		return []gameye.SessionListEntry{{ID: "b", Status: "running", Ports: map[string]int{"7360/tcp": 1}, Labels: labels}}, nil
	}
	fm, store, _ := newTestFleetManager(t, testConfig(), fapi)

	inst, err := fm.Get(context.Background(), "a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if inst.Metadata["mode"] != "duel" || inst.Metadata["env"] != nil {
		t.Fatalf("Get metadata = %v", inst.Metadata)
	}
	assertNoSecret(t, "storage after Get", store.raw("a"))

	list, _, err := fm.List(context.Background(), "", 10, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if list[0].Metadata["mode"] != "duel" || list[0].Metadata["env"] != nil {
		t.Fatalf("List metadata = %v", list[0].Metadata)
	}
	assertNoSecret(t, "storage after List", store.raw("b"))
	if store.raw("b") == "" {
		t.Fatalf("listed session not stored")
	}
	if labels["env"] == "" {
		t.Fatalf("the API response's labels were mutated")
	}
}

// --- join on create (U3) ---------------------------------------------------

func TestCreateJoinsUsers(t *testing.T) {
	fapi := newFakeApi()
	fm, store, handler := newTestFleetManager(t, testConfig(), fapi)

	out, err := fm.Create(context.Background(), 2, []string{"u1", "u2"}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	res := handler.wait(t)
	if res.status != runtime.CreateSuccess {
		t.Fatalf("status = %v, err = %v", res.status, res.err)
	}
	id := out[CreateSessionIdKey]

	joins := fapi.joinCalls()
	if len(joins) != 1 || joins[0].ID != id || strings.Join(joins[0].PlayerIDs, ",") != "u1,u2" {
		t.Fatalf("joins = %+v, want one call with both users", joins)
	}
	if len(res.sessions) != 2 || res.sessions[0].UserId != "u1" || res.sessions[1].UserId != "u2" ||
		res.sessions[0].SessionId != id || res.sessions[1].SessionId != id {
		t.Fatalf("session info = %+v", res.sessions)
	}
	if res.instance.PlayerCount != 2 {
		t.Fatalf("player count = %d", res.instance.PlayerCount)
	}
	if got := store.stored(t, id); got == nil || got.PlayerCount != 2 {
		t.Fatalf("stored = %+v, want player count 2", got)
	}
}

func TestCreateWithoutUsersDoesNotJoin(t *testing.T) {
	fapi := newFakeApi()
	fm, _, handler := newTestFleetManager(t, testConfig(), fapi)
	if _, err := fm.Create(context.Background(), 2, nil, nil, nil, nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	res := handler.wait(t)
	if res.sessions != nil {
		t.Fatalf("session info = %v, want nil without user ids", res.sessions)
	}
	if len(fapi.joinCalls()) != 0 {
		t.Fatalf("join called without user ids")
	}
}

func TestCreateJoinFailureStopsSession(t *testing.T) {
	fapi := newFakeApi()
	fapi.joinFn = func(ctx context.Context, req gameye.SessionJoin) ([]string, error) {
		return nil, &gameye.ApiError{StatusCode: 403}
	}
	fm, store, handler := newTestFleetManager(t, testConfig(), fapi)

	out, err := fm.Create(context.Background(), 2, []string{"u1", "u2"}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	res := handler.wait(t)
	if res.status != runtime.CreateError || !errors.Is(res.err, gameye.ErrForbidden) {
		t.Fatalf("status = %v, err = %v; want CreateError wrapping ErrForbidden", res.status, res.err)
	}
	if res.instance != nil || res.sessions != nil {
		t.Fatalf("instance and sessions must be nil on error")
	}
	if got := fapi.stopCalls(); len(got) != 1 || got[0] != out[CreateSessionIdKey] {
		t.Fatalf("stops = %v, want the started session stopped", got)
	}
	if store.stored(t, out[CreateSessionIdKey]) != nil {
		t.Fatalf("session must not be stored")
	}
}

// Gameye returns labels as observed on the self-serve API (2026-10-08): the
// caller's labels nested as a JSON string under "tags", the container env
// under "env" and platform data under "gameye".
func TestMetadataFromObservedLabelShape(t *testing.T) {
	labels := map[string]string{
		"env":    `{"SEAT_SECRET":"` + secret + `","GAMEYE_REGION":"eu-central-1"}`,
		"gameye": `{"gameye.io/organization":"acme"}`,
		"tags":   `{"mode":"duel","map":"scrapyard"}`,
	}
	got := metadataFromLabels(labels)
	if len(got) != 2 || got["mode"] != "duel" || got["map"] != "scrapyard" {
		t.Fatalf("metadata = %v, want only the caller's labels", got)
	}
}

func TestMetadataFromLabelsWithMalformedTags(t *testing.T) {
	got := metadataFromLabels(map[string]string{"tags": "not json", "gameye": "{}", "env": "{}"})
	if len(got) != 0 {
		t.Fatalf("metadata = %v, want empty", got)
	}
}
